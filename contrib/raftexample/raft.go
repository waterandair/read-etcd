// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"go.etcd.io/etcd/etcdserver/api/rafthttp"
	"go.etcd.io/etcd/etcdserver/api/snap"
	stats "go.etcd.io/etcd/etcdserver/api/v2stats"
	"go.etcd.io/etcd/pkg/fileutil"
	"go.etcd.io/etcd/pkg/types"
	"go.etcd.io/etcd/raft"
	"go.etcd.io/etcd/raft/raftpb"
	"go.etcd.io/etcd/wal"
	"go.etcd.io/etcd/wal/walpb"

	"go.uber.org/zap"
)

// A key-value stream backed by raft
// 对 etcd-raft 模块的一层封装，对上层模块提供了更加简介，方便的调用方式。
type raftNode struct {
	// 添加键值对数据，收到 http PUT 请求后，httpKVAPI 会将请求中的键值信息通过 proposeC 通道传给 raftNode 进行处理
	proposeC <-chan string // proposed messages (k,v)

	// http POST 请求表示修改集群节点的请求，
	confChangeC <-chan raftpb.ConfChange // proposed cluster config changes

	// 在创建 raftNode 后会返回 commitC，errorC， snapshotterReady 三个通道
	// raftNode 会将 etcd-raft 模块返回的待应用 Entry 记录写入 commitC 通道，另一方面，kvstore 会动 CommitC 通道中读取这些待应用的 Entry 记录并保存其中的键值对信息
	commitC chan<- *string // entries committed to log (k,v)

	// 当 etcd-raft 模块关闭或出现异常的时候，会通过 errorC 通道将该信息通知上层模块
	errorC chan<- error // errors from raft session

	// 记录当前节点ID
	id int // client ID for raft session

	// 当前集群中所有节点的地址，当前节点会通过该字段中保存的地址向集群中其他节点发送消息
	peers []string // raft peer URLs

	// 表示当前节点是否是后续加入到一个集群的节点
	join bool // node is joining an existing cluster

	// 存放 WAL 日志文件的目录
	waldir string // path to WAL directory

	// 存放快照文件的目录
	snapdir string // path to snapshot directory

	// 用于获取快照数据.
	getSnapshot func() ([]byte, error)

	// 在节点启动，回放 WAL 日志结束之后，记录最后一条 Entry 记录的索引值
	lastIndex uint64 // index of log at start

	// 记录当前集群的状态，从 node.confstatec 总获取的
	confState raftpb.ConfState

	// 保存当前快照所包含的最后一条 Entry 记录的索引值
	snapshotIndex uint64

	// 保存上层模块已应用的位置，即最后一条 Entry 记录的索引值
	appliedIndex uint64

	// raft backing for the commit/error channel
	// etcd-raft 中的 node
	node raft.Node

	// raftLog.storage 的一个基于内存大的实现
	raftStorage *raft.MemoryStorage

	// 负责 WAL 日志的管理。、
	// 当节点收到一条 Entry 记录时，首先将其保存到 raftLog.unstable 中，
	// 之后会将其封装到 Ready 实例中并交给上层模块发送给集群中其他节点，并完成持久化
	wal *wal.WAL

	// 负责管理快照数据，etcd-raft 模块并没有完成快照数据的管理，而是将其独立成一个单独的模块
	snapshotter *snap.Snapshotter

	// 用于通知上层模块 snapshotter 实例是否已经创建完成
	snapshotterReady chan *snap.Snapshotter // signals when snapshotter is ready

	// 两次生成快照之间间隔的 Entry 记录数，即当前节点每处理一定量的 Entry 记录，就触发一次快照数据的创建。
	// 每次生成快照时，即可释放一定量的 WAL 日志及 raftLog 中保存的 Entry 记录，从而避免大量 Entry 记录带来的
	// 内存压力及大量的 WAL 日志文件带来的磁盘压力。另外，定期创建快照也能减少节点重启时回放 WAL 日志数量，加速了启动速度。
	snapCount uint64

	// 节点待发送的消息只是记录到了 raft.msgs 中，etcd-raft 并没有提供网络层的实现，而是由上层模块决定两个节点之间如何通信。
	// 这样可以给通信层的实现带了更好的灵活性，比如如果两个节点在一个服务器中，完全可以使用共享内存的方式实现节点间的通信
	transport *rafthttp.Transport

	stopc chan struct{} // signals proposal channel closed

	httpstopc chan struct{} // signals http server to shutdown

	httpdonec chan struct{} // signals http server shutdown complete
}

var defaultSnapshotCount uint64 = 10000

// newRaftNode initiates a raft instance and returns a committed log entry
// channel and error channel. Proposals for log updates are sent over the
// provided the proposal channel. All log entries are replayed over the
// commit channel, followed by a nil message (to indicate the channel is
// current), then new log entries. To shutdown, close proposeC and read errorC.
//
func newRaftNode(
	id int,
	peers []string,
	join bool,
	getSnapshot func() ([]byte, error),
	proposeC <-chan string,
	confChangeC <-chan raftpb.ConfChange,
) (<-chan *string, <-chan error, <-chan *snap.Snapshotter) {

	commitC := make(chan *string)
	errorC := make(chan error)

	rc := &raftNode{
		proposeC:    proposeC,
		confChangeC: confChangeC,
		commitC:     commitC,
		errorC:      errorC,
		id:          id,
		peers:       peers,
		join:        join,
		// 初始化存放 WAL 日志和 SnapShot 文件的目录
		waldir:      fmt.Sprintf("raftexample-%d", id),
		snapdir:     fmt.Sprintf("raftexample-%d-snap", id),
		getSnapshot: getSnapshot,
		snapCount:   defaultSnapshotCount, // 默认 10000
		stopc:       make(chan struct{}),
		httpstopc:   make(chan struct{}),
		httpdonec:   make(chan struct{}),

		snapshotterReady: make(chan *snap.Snapshotter, 1),
		// rest of structure populated after WAL replay 其余字段在 WAL 日志回放完成之后才会初始化
	}

	// 单独启动一个 goroutine 执行 startRaft() 方法，在该方法中完成剩余初始化操作
	go rc.startRaft()
	return commitC, errorC, rc.snapshotterReady
}

func (rc *raftNode) saveSnap(snap raftpb.Snapshot) error {
	// must save the snapshot index to the WAL before saving the
	// snapshot to maintain the invariant that we only Open the
	// wal at previously-saved snapshot indexes.
	// 根据快照元数据，创建 walpb.Snapshot 实例
	walSnap := walpb.Snapshot{
		Index: snap.Metadata.Index,
		Term:  snap.Metadata.Term,
	}

	// WAL 将快照元数据封装成一条日志记录下来
	if err := rc.wal.SaveSnapshot(walSnap); err != nil {
		return err
	}

	// 将新快照数据写入快照文件
	if err := rc.snapshotter.SaveSnap(snap); err != nil {
		return err
	}

	// 根据快照的元数据，释放掉一些无用的 WAL 日志文件的句柄
	return rc.wal.ReleaseLockTo(snap.Metadata.Index)
}

func (rc *raftNode) entriesToApply(ents []raftpb.Entry) (nents []raftpb.Entry) {
	if len(ents) == 0 {
		return ents
	}
	firstIdx := ents[0].Index
	if firstIdx > rc.appliedIndex+1 {
		log.Fatalf("first index of committed entry[%d] should <= progress.appliedIndex[%d]+1", firstIdx, rc.appliedIndex)
	}
	// 过滤掉已被应用过的 Entry 记录
	if rc.appliedIndex-firstIdx+1 < uint64(len(ents)) {
		nents = ents[rc.appliedIndex-firstIdx+1:]
	}
	return nents
}

// publishEntries writes committed log entries to commit channel and returns
// whether all entries could be published.
// 将待应用的 Entry 记录写入 commitC 通道中，后续 kvstore 就可以读取 commitC 通道并保存响应的键值对数据d
func (rc *raftNode) publishEntries(ents []raftpb.Entry) bool {
	for i := range ents {
		switch ents[i].Type {
		case raftpb.EntryNormal:
			// 如果 entry.data 的值为空，直接忽略
			if len(ents[i].Data) == 0 {
				// ignore empty messages
				break
			}
			s := string(ents[i].Data)
			select {
			case rc.commitC <- &s: // 将数据写入通道， kvstore 会从中读取并记录
			case <-rc.stopc:
				return false
			}

		case raftpb.EntryConfChange:
			// 底层 etcd-raft 组件进行处理
			var cc raftpb.ConfChange
			cc.Unmarshal(ents[i].Data)
			rc.confState = *rc.node.ApplyConfChange(cc)

			// 除了底层 etcd-raft 需要做处理，网络层也需要做响应的处理，添加或删除 Peer 实例
			switch cc.Type {
			case raftpb.ConfChangeAddNode:
				if len(cc.Context) > 0 {
					rc.transport.AddPeer(types.ID(cc.NodeID), []string{string(cc.Context)})
				}
			case raftpb.ConfChangeRemoveNode:
				if cc.NodeID == uint64(rc.id) {
					log.Println("I've been removed from the cluster! Shutting down.")
					return false
				}
				rc.transport.RemovePeer(types.ID(cc.NodeID))
			}
		}

		// after commit, update appliedIndex
		// 处理完成后，更新 raftNode 记录的已提交位置
		rc.appliedIndex = ents[i].Index

		// special nil commit to signal replay has finished
		// 此次应用是否为重放的 entry记录，如果是且重放完成，则使用 commitC 通知 kvstore
		if ents[i].Index == rc.lastIndex {
			select {
			case rc.commitC <- nil: // TODO: question
			case <-rc.stopc:
				return false
			}
		}
	}
	return true
}

func (rc *raftNode) loadSnapshot() *raftpb.Snapshot {
	snapshot, err := rc.snapshotter.Load()
	if err != nil && err != snap.ErrNoSnapshot {
		log.Fatalf("raftexample: error loading snapshot (%v)", err)
	}
	return snapshot
}

// openWAL returns a WAL ready for reading.
func (rc *raftNode) openWAL(snapshot *raftpb.Snapshot) *wal.WAL {
	if !wal.Exist(rc.waldir) {
		if err := os.Mkdir(rc.waldir, 0750); err != nil {
			log.Fatalf("raftexample: cannot create dir for wal (%v)", err)
		}

		w, err := wal.Create(zap.NewExample(), rc.waldir, nil)
		if err != nil {
			log.Fatalf("raftexample: create wal error (%v)", err)
		}
		w.Close()
	}

	// 创建 walsnap.Snapshot 实例并初始化 Index 和 Term
	walsnap := walpb.Snapshot{}
	if snapshot != nil {
		walsnap.Index, walsnap.Term = snapshot.Metadata.Index, snapshot.Metadata.Term
	}
	log.Printf("loading WAL at term %d and index %d", walsnap.Term, walsnap.Index)
	w, err := wal.Open(zap.NewExample(), rc.waldir, walsnap)
	if err != nil {
		log.Fatalf("raftexample: error loading wal (%v)", err)
	}

	return w
}

// replayWAL replays WAL entries into the raft instance.
func (rc *raftNode) replayWAL() *wal.WAL {
	log.Printf("replaying WAL of member %d", rc.id)
	// 读取快照文件
	snapshot := rc.loadSnapshot()
	// 根据快照元数据创建 WAL 实例
	w := rc.openWAL(snapshot)
	// 读取快照文件之后的全部 WAL 日志数据，并获取动态信息
	_, st, ents, err := w.ReadAll()
	if err != nil {
		log.Fatalf("raftexample: failed to read WAL (%v)", err)
	}
	rc.raftStorage = raft.NewMemoryStorage()
	if snapshot != nil {
		rc.raftStorage.ApplySnapshot(*snapshot) // 将快照数据加载到 MemoryStorage 中
	}
	rc.raftStorage.SetHardState(st) // 将读取 wal 日志之后得到的 hardstate 加载到 MemoryStorage 中

	// append to storage so raft starts at the right place in log
	rc.raftStorage.Append(ents) // 将读取 wal 日志得到的 entries 记录加载到 MemoryStorage 中

	// send nil once lastIndex is published so client knows commit channel is current
	// 快照之后存在已经持久化的 Entry 记录，这些记录需要回放到上层应用的状态机中
	if len(ents) > 0 {
		rc.lastIndex = ents[len(ents)-1].Index // 记录回放的位置
	} else {
		// 快照之后不存在已经持久化的 Entry 记录，则向 commitC 中写入 nil，
		// 当 wal 日志全部回放完成之后，也会向 commitC 中写入 nil 作为信号
		rc.commitC <- nil
	}
	return w
}

func (rc *raftNode) writeError(err error) {
	rc.stopHTTP()
	close(rc.commitC)
	rc.errorC <- err
	close(rc.errorC)
	rc.node.Stop()
}

// 3. 创建 raft.Config 实例，其中包含了启动 etcd-raft 模块的所有配置
// 4. 初始化底层 etcd-raft 模块，得到 node 实例
// 5. 创建 Transport 实例，负责各个节点间的网络通信，在 rafthttp 包中实现
// 6. 建立与集群总其他节点的网络连接
// 7. 启动网络组件， 其中会监听当前节点与集群中各个其他节点的网络连接，并进行节点之间的消息读写
// 8. 启动两个后台 goroutine， 处理上层模块与底层 etcd-raft 模块的交互
func (rc *raftNode) startRaft() {
	// 检测快照数据目录是否村存在
	if !fileutil.Exist(rc.snapdir) {
		if err := os.Mkdir(rc.snapdir, 0750); err != nil {
			log.Fatalf("raftexample: cannot create dir for snapshot (%v)", err)
		}
	}
	//  创建 Snapshotter，它提供了快照管理能力， 通过 rc.snapshotterReady 返回给上层模块
	rc.snapshotter = snap.New(zap.NewExample(), rc.snapdir)
	rc.snapshotterReady <- rc.snapshotter

	// 2. 创建 WAL 实例，加载快照并回访 WAL 日志
	oldwal := wal.Exist(rc.waldir) // 检测 waldir 目录下是否存在旧的 wal 日志文件
	rc.wal = rc.replayWAL()        // 先加载快照数据，然后重放 wal 日志文件

	// 3. 创建 raft.Config 实例
	rpeers := make([]raft.Peer, len(rc.peers))
	for i := range rpeers {
		rpeers[i] = raft.Peer{ID: uint64(i + 1)}
	}
	c := &raft.Config{
		ID:                        uint64(rc.id),
		ElectionTick:              10,             // election_timeout
		HeartbeatTick:             1,              // heartbeat_timeout
		Storage:                   rc.raftStorage, // 持久化存储，此处为 MemoryStorage
		MaxSizePerMsg:             1024 * 1024,    // 每条消息的最大长度
		MaxInflightMsgs:           256,            // 已发送但未收到响应的消息个数上限
		MaxUncommittedEntriesSize: 1 << 30,
	}

	// 4. 判断是首次启动还是重启， 初始化 etcd-raft 模块
	if oldwal {
		// 重启
		rc.node = raft.RestartNode(c)
	} else {
		startPeers := rpeers
		if rc.join {
			startPeers = nil
		}
		// 首次启动
		rc.node = raft.StartNode(c, startPeers)
	}

	// 5. 创建 Transport 实例并启动
	rc.transport = &rafthttp.Transport{
		Logger:      zap.NewExample(),
		ID:          types.ID(rc.id),
		ClusterID:   0x1000,
		Raft:        rc,
		ServerStats: stats.NewServerStats("", ""),
		LeaderStats: stats.NewLeaderStats(strconv.Itoa(rc.id)),
		ErrorC:      make(chan error),
	}

	rc.transport.Start() // 启动网络服务相关组件
	for i := range rc.peers {
		if i+1 != rc.id {
			rc.transport.AddPeer(types.ID(i+1), []string{rc.peers[i]})
		}
	}

	// 启动一个 goroutine， 其中会监听当前节点与集群中其他节点之间的网络连接
	go rc.serveRaft()
	// 处理上层应用与底层 etcd-raft 模块的交互
	go rc.serveChannels()
}

// stop closes http, closes all channels, and stops raft.
func (rc *raftNode) stop() {
	rc.stopHTTP()
	close(rc.commitC)
	close(rc.errorC)
	rc.node.Stop()
}

func (rc *raftNode) stopHTTP() {
	rc.transport.Stop()
	close(rc.httpstopc)
	<-rc.httpdonec
}

func (rc *raftNode) publishSnapshot(snapshotToSave raftpb.Snapshot) {
	if raft.IsEmptySnap(snapshotToSave) {
		return
	}

	log.Printf("publishing snapshot at index %d", rc.snapshotIndex)
	defer log.Printf("finished publishing snapshot at index %d", rc.snapshotIndex)

	if snapshotToSave.Metadata.Index <= rc.appliedIndex {
		log.Fatalf("snapshot index [%d] should > progress.appliedIndex [%d]", snapshotToSave.Metadata.Index, rc.appliedIndex)
	}

	// 使用 commitC 通道通知上层应用加载新生成的快照数据
	rc.commitC <- nil // trigger kvstore to load snapshot

	// 记录新快照的元数据
	rc.confState = snapshotToSave.Metadata.ConfState
	rc.snapshotIndex = snapshotToSave.Metadata.Index
	rc.appliedIndex = snapshotToSave.Metadata.Index
}

var snapshotCatchUpEntriesN uint64 = 10000

func (rc *raftNode) maybeTriggerSnapshot() {
	if rc.appliedIndex-rc.snapshotIndex <= rc.snapCount {
		return
	}

	log.Printf("start snapshot [applied index: %d | last snapshot index: %d]", rc.appliedIndex, rc.snapshotIndex)
	data, err := rc.getSnapshot()
	if err != nil {
		log.Panic(err)
	}
	snap, err := rc.raftStorage.CreateSnapshot(rc.appliedIndex, &rc.confState, data)
	if err != nil {
		panic(err)
	}
	if err := rc.saveSnap(snap); err != nil {
		panic(err)
	}

	compactIndex := uint64(1)
	if rc.appliedIndex > snapshotCatchUpEntriesN {
		compactIndex = rc.appliedIndex - snapshotCatchUpEntriesN
	}
	if err := rc.raftStorage.Compact(compactIndex); err != nil {
		panic(err)
	}

	log.Printf("compacted log at index %d", compactIndex)
	rc.snapshotIndex = rc.appliedIndex
}

func (rc *raftNode) serveChannels() {
	snap, err := rc.raftStorage.Snapshot()
	if err != nil {
		panic(err)
	}
	rc.confState = snap.Metadata.ConfState
	rc.snapshotIndex = snap.Metadata.Index
	rc.appliedIndex = snap.Metadata.Index

	defer rc.wal.Close()
	// 创建一个每个 100ms 触发一次的定时器，逻辑上， 100ms 就是 etcd-raft 中最小的时间单位，该定时器每触发一次，逻辑时钟就推进一次
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	// send proposals over raft
	// 单独启动一个 goroutine 负责将 propose，confChangeC 通道上接收到的数据传递给 etcd-raft 进行处理
	go func() {
		confChangeCount := uint64(0)

		for rc.proposeC != nil && rc.confChangeC != nil {
			select {
			case prop, ok := <-rc.proposeC: // 接收上层通过 proposeC 通道发来的数据
				if !ok {
					// 如果发生异常，置为空
					rc.proposeC = nil
				} else {
					// blocks until accepted by raft state machine
					// 将数据传入底层 etcd-raft 组件进行处理
					rc.node.Propose(context.TODO(), []byte(prop))
				}

			case cc, ok := <-rc.confChangeC:
				if !ok {
					rc.confChangeC = nil
				} else {
					confChangeCount++ // 统计集群变更请求的个数，并将其作为ID
					cc.ID = confChangeCount
					rc.node.ProposeConfChange(context.TODO(), cc)
				}
			}
		}
		// client closed channel; shutdown raft if not already
		close(rc.stopc)
	}()

	// 处理 etcd-raft 返回给上层模块的数据及其它相关的操作
	// event loop on raft state machine updates
	for {
		select {
		case <-ticker.C:
			// ticker 定时器触发一次（100ms），就会推进 etcd-raft 组件的逻辑时钟
			rc.node.Tick()

		// store raft entries to wal, then publish over commit channel
		// 从 node.readyc 通道中读取 etcd-raft 给上层模块返回的信息
		case rd := <-rc.node.Ready():
			// 将当前 etcd-raft 组件的状态信息，以及待持久化的 Entries 记录先记录到 WAL 日志中， 即使之后宕机，这些信息也可以在节点下次启动时，通过前面回放 WAL 日志的方式进行恢复
			rc.wal.Save(rd.HardState, rd.Entries)

			// 检测 etcd-raft 生成了新的快照
			if !raft.IsEmptySnap(rd.Snapshot) {
				// 将新的快照数据写入快照文件中
				rc.saveSnap(rd.Snapshot)
				// 将新快照持久化
				rc.raftStorage.ApplySnapshot(rd.Snapshot)
				// 通知上层应用加载新快照
				rc.publishSnapshot(rd.Snapshot)
			}

			// 将待持久化的 Entry 记录追加到 raftStorage 中完成持久化
			rc.raftStorage.Append(rd.Entries)
			// 将待发送的消息发送到指定节点
			rc.transport.Send(rd.Messages)
			// 将已提交，待应用的 Entry 记录应用到上层状态机中
			if ok := rc.publishEntries(rc.entriesToApply(rd.CommittedEntries)); !ok {
				rc.stop()
				return
			}

			// 随着节点的运行, WAL 日志量和 raftLog.storage 中的 Entry 记录会不断增加，所以节点每处理 10000 条 entry 记录，就会触发一次创建快照的过程，
			// 同时，WAL 会释放一些日志文件的句柄， raftLog.Storage 也会压缩其保存的 Entry 记录
			rc.maybeTriggerSnapshot()

			// 上层应用处理完该 Ready 实例，通知 etcd-raft 组件准备返回下一个 Ready 实例
			rc.node.Advance()

		case err := <-rc.transport.ErrorC: // 处理网络异常
			rc.writeError(err) // 关闭与集群中其他节点的网络连接
			return

		case <-rc.stopc: // 处理关闭命令
			rc.stop()
			return
		}
	}
}

func (rc *raftNode) serveRaft() {
	// 获取当前节点的 url 地址
	url, err := url.Parse(rc.peers[rc.id-1])
	if err != nil {
		log.Fatalf("raftexample: Failed parsing URL (%v)", err)
	}

	// 创建  StoppableListener 实例， 它实现了 net.Listener 接口，会与 http.Server 配合实现对当前节点的 url 地址进行监听
	ln, err := newStoppableListener(url.Host, rc.httpstopc)
	if err != nil {
		log.Fatalf("raftexample: Failed to listen rafthttp (%v)", err)
	}

	// 创建 http.Server 实例
	err = (&http.Server{Handler: rc.transport.Handler()}).Serve(ln)
	select {
	case <-rc.httpstopc:
	default:
		log.Fatalf("raftexample: Failed to serve rafthttp (%v)", err)
	}
	close(rc.httpdonec)
}

func (rc *raftNode) Process(ctx context.Context, m raftpb.Message) error {
	return rc.node.Step(ctx, m)
}
func (rc *raftNode) IsIDRemoved(id uint64) bool                           { return false }
func (rc *raftNode) ReportUnreachable(id uint64)                          {}
func (rc *raftNode) ReportSnapshot(id uint64, status raft.SnapshotStatus) {}
