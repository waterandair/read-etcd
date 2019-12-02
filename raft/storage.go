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

package raft

import (
	"errors"
	"sync"

	pb "go.etcd.io/etcd/raft/raftpb"
)

// ErrCompacted is returned by Storage.Entries/Compact when a requested
// index is unavailable because it predates the last snapshot.
var ErrCompacted = errors.New("requested index is unavailable due to compaction")

// ErrSnapOutOfDate is returned by Storage.CreateSnapshot when a requested
// index is older than the existing snapshot.
var ErrSnapOutOfDate = errors.New("requested index is older than the existing snapshot")

// ErrUnavailable is returned by Storage interface when the requested log entries
// are unavailable.
var ErrUnavailable = errors.New("requested entry at index is unavailable")

// ErrSnapshotTemporarilyUnavailable is returned by the Storage interface when the required
// snapshot is temporarily unavailable.
var ErrSnapshotTemporarilyUnavailable = errors.New("snapshot is temporarily unavailable")

// Storage is an interface that may be implemented by the application
// to retrieve log entries from storage.
//
// If any Storage method returns an error, the raft instance will
// become inoperable and refuse to participate in elections; the
// application is responsible for cleanup and recovery in this case.
type Storage interface {
	// TODO(tbg): split this into two interfaces, LogStorage and StateStorage.

	// InitialState returns the saved HardState and ConfState information.

	// 返回 Storage 中记录的状态信息,返回的是 HardState 实例和 ConfState 实例
	// HardState 中存储了节点的状态信息,包含 Term (当前任期号),Vote (当前任期将选票投给了那个目标节点), Commit(最后一条已提交的Entry的索引值)
	// ConfState 中存储了当前集群中所有节点的 ID
	InitialState() (pb.HardState, pb.ConfState, error)

	// Entries returns a slice of log entries in the range [lo,hi).
	// MaxSize limits the total size of the log entries returned, but
	// Entries returns at least one entry if any.
	// Storage 存储了当前节点所有的 Entry 节点, Entries 返回制定范围的 Entry 记录, MaxSize 限制了返回的最大字节数,但如果有多条 Entry 的话,最少会返回一条
	Entries(lo, hi, maxSize uint64) ([]pb.Entry, error)

	// Term returns the term of entry i, which must be in the range
	// [FirstIndex()-1, LastIndex()]. The term of the entry before
	// FirstIndex is retained for matching purposes even though the
	// rest of that entry may not be available.
	// 返回指定索引的 Entry 的任期值.
	Term(i uint64) (uint64, error)

	// LastIndex returns the index of the last entry in the log.
	// 返回 Storage 中记录的最后一条 Entry 的索引值
	LastIndex() (uint64, error)

	// FirstIndex returns the index of the first log entry that is
	// possibly available via Entries (older entries have been incorporated
	// into the latest Snapshot; if storage only contains the dummy entry the
	// first log entry is not available).
	// 返回 Storage 中记录的第一条 Entry 的索引值,在它之前所有的 Entry 都已经被提交进了最近一次的 SnapShot 中.
	FirstIndex() (uint64, error)

	// Snapshot returns the most recent snapshot.
	// If snapshot is temporarily unavailable, it should return ErrSnapshotTemporarilyUnavailable,
	// so raft state machine could know that Storage needs some time to prepare
	// snapshot and call Snapshot later.
	// 返回最近一次生成快照的数据
	Snapshot() (pb.Snapshot, error)
}

// MemoryStorage implements the Storage interface backed by an in-memory array.
// MemoryStorage 是 Storage 接口的实现,它的底层存储是基于内存的.
type MemoryStorage struct {
	// Protects access to all fields. Most methods of MemoryStorage are
	// run on the raft goroutine, but Append() is run on an application
	// goroutine.
	sync.Mutex // 对所有字段的读写操作,都要加锁保护.

	hardState pb.HardState
	snapshot  pb.Snapshot
	// ents[i] has raft log position i+snapshot.Metadata.Index
	ents []pb.Entry
}

// NewMemoryStorage creates an empty MemoryStorage.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		// When starting from scratch populate the list with a dummy entry at term zero.
		// 在 0 的位置填充一个虚拟的 Entry
		// 这个位置是留给快照的最后一个 Entry 的
		ents: make([]pb.Entry, 1),
	}
}

// InitialState implements the Storage interface.
func (ms *MemoryStorage) InitialState() (pb.HardState, pb.ConfState, error) {
	return ms.hardState, ms.snapshot.Metadata.ConfState, nil
}

// SetHardState saves the current HardState.
func (ms *MemoryStorage) SetHardState(st pb.HardState) error {
	ms.Lock()
	defer ms.Unlock()
	ms.hardState = st
	return nil
}

// Entries implements the Storage interface.
func (ms *MemoryStorage) Entries(lo, hi, maxSize uint64) ([]pb.Entry, error) {
	ms.Lock()
	defer ms.Unlock()
	offset := ms.ents[0].Index
	if lo <= offset {
		return nil, ErrCompacted
	}

	if hi > ms.lastIndex()+1 {
		raftLogger.Panicf("entries' hi(%d) is out of bound lastindex(%d)", hi, ms.lastIndex())
	}
	// only contains dummy entries.   // suggestion: 这个判断放到最前面
	if len(ms.ents) == 1 {
		return nil, ErrUnavailable
	}

	ents := ms.ents[lo-offset : hi-offset]
	return limitSize(ents, maxSize), nil
}

// Term implements the Storage interface.
// 注意只能取到不在快照中的索引的 term
func (ms *MemoryStorage) Term(i uint64) (uint64, error) {
	ms.Lock()
	defer ms.Unlock()
	offset := ms.ents[0].Index
	if i < offset {
		return 0, ErrCompacted
	}
	if int(i-offset) >= len(ms.ents) {
		return 0, ErrUnavailable
	}
	return ms.ents[i-offset].Term, nil
}

// LastIndex implements the Storage interface.
func (ms *MemoryStorage) LastIndex() (uint64, error) {
	ms.Lock()
	defer ms.Unlock()
	return ms.lastIndex(), nil
}

func (ms *MemoryStorage) lastIndex() uint64 {
	return ms.ents[0].Index + uint64(len(ms.ents)) - 1
}

// FirstIndex implements the Storage interface.
// Storage 中存储的第一个 Entry 的索引值,注意是 entrys[0] 位置的 entry 的索引值 + 1 的值
func (ms *MemoryStorage) FirstIndex() (uint64, error) {
	ms.Lock()
	defer ms.Unlock()
	return ms.firstIndex(), nil
}

func (ms *MemoryStorage) firstIndex() uint64 {
	return ms.ents[0].Index + 1
}

// Snapshot implements the Storage interface.
func (ms *MemoryStorage) Snapshot() (pb.Snapshot, error) {
	ms.Lock()
	defer ms.Unlock()
	return ms.snapshot, nil
}

// ApplySnapshot overwrites the contents of this Storage object with
// those of the given snapshot.
// 更新快照
func (ms *MemoryStorage) ApplySnapshot(snap pb.Snapshot) error {
	ms.Lock()
	defer ms.Unlock()

	// handle check for old snapshot being applied
	msIndex := ms.snapshot.Metadata.Index // 旧快照中最后一条 Entry 记录的索引值
	snapIndex := snap.Metadata.Index      // 新快照中最后一条 Entry 记录的索引值
	// 快照的最后一条索引只增不减
	if msIndex >= snapIndex {
		return ErrSnapOutOfDate
	}

	ms.snapshot = snap
	ms.ents = []pb.Entry{{Term: snap.Metadata.Term, Index: snap.Metadata.Index}}
	return nil
}

// CreateSnapshot makes a snapshot which can be retrieved with Snapshot() and
// can be used to reconstruct the state at that point.
// If any configuration changes have been made since the last compaction,
// the result of the last ApplyConfChange must be passed in.
// 参数 i 是新建快照包含的最大索引值, cs 表示当前集群的状态, data 是新建 snapshot 的具体数据.
// 如果上次建立快照之后,配置发生了改变,需要将最后一次 ApplyConfChange 的结果传入
func (ms *MemoryStorage) CreateSnapshot(i uint64, cs *pb.ConfState, data []byte) (pb.Snapshot, error) {
	ms.Lock()
	defer ms.Unlock()

	// 新建的快照的最后一个索引, 必须大于当前快照中最后一个索引, 必须小于当前本地的最大索引
	if i <= ms.snapshot.Metadata.Index {
		return pb.Snapshot{}, ErrSnapOutOfDate
	}

	if i > ms.lastIndex() {
		raftLogger.Panicf("snapshot %d is out of bound lastindex(%d)", i, ms.lastIndex())
	}

	offset := ms.ents[0].Index // 上一次快照的最后一个索引

	ms.snapshot.Metadata.Index = i                     // 更新当前快照的最后一个 Entry 的索引值
	ms.snapshot.Metadata.Term = ms.ents[i-offset].Term // 更新当前快照的最后一个 Entry 的任期值
	if cs != nil {
		ms.snapshot.Metadata.ConfState = *cs // 更新当前集群的状态
	}
	ms.snapshot.Data = data // 更新具体的快照数据
	return ms.snapshot, nil
	// 新建快照后,一般会接着调用 func (ms *MemoryStorage) Compact(compactIndex uint64) 方法将本地 Entries 中已存入快照中的索引抛弃,
	// 以此实现压缩本地日志的效果
}

// Compact discards all log entries prior to compactIndex.
// It is the application's responsibility to not attempt to compact an index
// greater than raftLog.applied.
// Compact 丢弃所有  compactIndex 及之前所有的 entries
// 这里不保证 compactIndex <= raftLog.applied(todo 应用到状态机的索引位置), 这个应该由应用程序去保证
func (ms *MemoryStorage) Compact(compactIndex uint64) error {
	ms.Lock()
	defer ms.Unlock()
	offset := ms.ents[0].Index
	if compactIndex <= offset {
		return ErrCompacted
	}
	if compactIndex > ms.lastIndex() {
		raftLogger.Panicf("compact %d is out of bound lastindex(%d)", compactIndex, ms.lastIndex())
	}

	i := compactIndex - offset
	ents := make([]pb.Entry, 1, 1+uint64(len(ms.ents))-i)
	ents[0].Index = ms.ents[i].Index
	ents[0].Term = ms.ents[i].Term
	ents = append(ents, ms.ents[i+1:]...)
	ms.ents = ents
	return nil
}

// Append the new entries to storage.
// TODO (xiangli): ensure the entries are continuous and
// entries[0].Index > ms.entries[0].Index
func (ms *MemoryStorage) Append(entries []pb.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	ms.Lock()
	defer ms.Unlock()

	first := ms.firstIndex()                            // 当前第一条 entry 的索引值, 注意是 ms.ents[1].Index
	last := entries[0].Index + uint64(len(entries)) - 1 // 传进来的 entries 的最后一条 entry 的索引值, 写成这样应该好理解一点 last := entries[len(entries)-1].Index

	// shortcut if there is no new entry.
	// 保证传进来的 entries 包含新的日志
	if last < first {
		return nil
	}
	// truncate compacted entries
	// first 索引位置之前的 Entry 都已经存入了快照
	// 如果传入的 entries 中包含快照中的 entry, 要将其忽略
	if first > entries[0].Index {
		entries = entries[first-entries[0].Index:]
	}

	// 传入的 entries 中第一条可用的 Entry 与 first 位置之间的差距
	offset := entries[0].Index - ms.ents[0].Index
	switch {
	case uint64(len(ms.ents)) > offset:
		// 如果传进来的 entries 中包含本地已经存在的 entry, 要用新传进来的 entries 替换掉旧的 entries
		// 注意: 这一步是在理解概念中比较容易混淆的地方; 只要日志没有被存到快照中,它就可以被新传进来的相同索引的 entry 覆盖
		ms.ents = append([]pb.Entry{}, ms.ents[:offset]...)
		ms.ents = append(ms.ents, entries...)
	case uint64(len(ms.ents)) == offset:
		// 正常情况下,都是走到这一步,将传进来的日志直接追加到已有日志中.
		ms.ents = append(ms.ents, entries...)
	default:
		// 传进来的 entries 与已有的 entries 不能连续, 不能追加
		raftLogger.Panicf("missing log entry [last: %d, append at: %d]",
			ms.lastIndex(), entries[0].Index)
	}
	return nil
}
