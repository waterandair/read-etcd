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
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"go.etcd.io/etcd/raft/confchange"
	"go.etcd.io/etcd/raft/quorum"
	pb "go.etcd.io/etcd/raft/raftpb"
	"go.etcd.io/etcd/raft/tracker"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0
const noLimit = math.MaxUint64

// Possible values for StateType.
const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
	StatePreCandidate
	numStates
)

type ReadOnlyOption int

const (
	// ReadOnlySafe guarantees the linearizability of the read only request by
	// communicating with the quorum. It is the default and suggested option.
	ReadOnlySafe ReadOnlyOption = iota
	// ReadOnlyLeaseBased ensures linearizability of the read only request by
	// relying on the leader lease. It can be affected by clock drift.
	// If the clock drift is unbounded, leader might keep the lease longer than it
	// should (clock can move backward/pause without any bound). ReadIndex is not safe
	// in that case.
	ReadOnlyLeaseBased
)

// Possible values for CampaignType
const (
	// campaignPreElection represents the first phase of a normal election when
	// Config.PreVote is true.
	campaignPreElection CampaignType = "CampaignPreElection"
	// campaignElection represents a normal (time-based) election (the second phase
	// of the election when Config.PreVote is true).
	campaignElection CampaignType = "CampaignElection"
	// campaignTransfer represents the type of leader transfer
	campaignTransfer CampaignType = "CampaignTransfer"
)

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// lockedRand is a small wrapper around rand.Rand to provide
// synchronization among multiple raft groups. Only the methods needed
// by the code are exposed (e.g. Intn).
type lockedRand struct {
	mu   sync.Mutex
	rand *rand.Rand
}

func (r *lockedRand) Intn(n int) int {
	r.mu.Lock()
	v := r.rand.Intn(n)
	r.mu.Unlock()
	return v
}

var globalRand = &lockedRand{
	rand: rand.New(rand.NewSource(time.Now().UnixNano())),
}

// CampaignType represents the type of campaigning
// the reason we use the type of string instead of uint64
// is because it's simpler to compare and fill in raft entries
type CampaignType string

// StateType represents the role of a node in a cluster.
type StateType uint64

var stmap = [...]string{
	"StateFollower",     // 跟随者
	"StateCandidate",    // 候选人
	"StateLeader",       // 领导者
	"StatePreCandidate", // todo 在 Follower 节点超时准备进入候选人前, 为了避免因为网络分区或其他原因引起的循环选举,
	// 要在这里先检查当前节点是否和和超过半数的节点通信,此时节点的状态为 StatePreCandidate
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64 // 当前节点的 ID

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64 // 集群中所有节点的 ID

	// learners contains the IDs of all learner nodes (including self if the
	// local node is a learner) in the raft cluster. learners only receives
	// entries from the leader node. It does not vote or promote itself.
	learners []uint64 // TODO learners

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int // 选举超时时间
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int // 心跳间隔时间

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage // 保存日志的接口
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64 // 当前节点已经应用的最后一条 Entry 记录的索引值,这个值在重启时需要设置,否则会重新应用已经应用过的 Entry 记录

	// MaxSizePerMsg limits the max byte size of each append message. Smaller
	// value lowers the raft recovery cost(initial probing and message lost
	// during normal operation). On the other side, it might affect the
	// throughput during normal replication. Note: math.MaxUint64 for unlimited,
	// 0 for at most one entry per message.
	MaxSizePerMsg uint64 // 每条消息的最大字节数, math.MaxUint64 表示没有上限, 0 表示每条消息最多携带一条 Entry
	// MaxCommittedSizePerReady limits the size of the committed entries which
	// can be applied.
	MaxCommittedSizePerReady uint64
	// MaxUncommittedEntriesSize limits the aggregate byte size of the
	// uncommitted entries that may be appended to a leader's log. Once this
	// limit is exceeded, proposals will begin to return ErrProposalDropped
	// errors. Note: 0 for no limit.
	MaxUncommittedEntriesSize uint64
	// MaxInflightMsgs limits the max number of in-flight append messages during
	// optimistic replication phase. The application transportation layer usually
	// has its own sending buffer over TCP/UDP. Setting MaxInflightMsgs to avoid
	// overflowing that sending buffer. TODO (xiangli): feedback to application to
	// limit the proposal rate?
	MaxInflightMsgs int // 阈值: 已发出但未收到响应的最大消息个数

	// CheckQuorum specifies if the leader should check quorum activity. Leader
	// steps down when quorum is not active for an electionTimeout.
	CheckQuorum bool // 是否开启 CheckQuorum, leader 定义检查能不能和至少半数的 Follower 通信

	// PreVote enables the Pre-Vote algorithm described in raft thesis section
	// 9.6. This prevents disruption when a node that has been partitioned away
	// rejoins the cluster.
	PreVote bool // 是否开启 PreVote 模式, 在发起选举前检查是否可以和至少半数的 Follower 节点通信

	// ReadOnlyOption specifies how the read only request is processed.
	//
	// ReadOnlySafe guarantees the linearizability of the read only request by
	// communicating with the quorum. It is the default and suggested option.
	//
	// ReadOnlyLeaseBased ensures linearizability of the read only request by
	// relying on the leader lease. It can be affected by clock drift.
	// If the clock drift is unbounded, leader might keep the lease longer than it
	// should (clock can move backward/pause without any bound). ReadIndex is not safe
	// in that case.
	// CheckQuorum MUST be enabled if ReadOnlyOption is ReadOnlyLeaseBased.
	ReadOnlyOption ReadOnlyOption // todo

	// Logger is the logger used for raft log. For multinode which can host
	// multiple raft group, each raft group can have its own logger
	Logger Logger

	// DisableProposalForwarding set to true means that followers will drop
	// proposals, rather than forwarding them to the leader. One use case for
	// this feature would be in a situation where the Raft leader is used to
	// compute the data of a proposal, for example, adding a timestamp from a
	// hybrid logical clock to data in a monotonically increasing way. Forwarding
	// should be disabled to prevent a follower with an inaccurate hybrid
	// logical clock from assigning the timestamp and then forwarding the data
	// to the leader.
	DisableProposalForwarding bool
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	if c.MaxUncommittedEntriesSize == 0 {
		c.MaxUncommittedEntriesSize = noLimit
	}

	// default MaxCommittedSizePerReady to MaxSizePerMsg because they were
	// previously the same parameter.
	if c.MaxCommittedSizePerReady == 0 {
		c.MaxCommittedSizePerReady = c.MaxSizePerMsg
	}

	if c.MaxInflightMsgs <= 0 {
		return errors.New("max inflight messages must be greater than 0")
	}

	if c.Logger == nil {
		c.Logger = raftLogger
	}

	if c.ReadOnlyOption == ReadOnlyLeaseBased && !c.CheckQuorum {
		return errors.New("CheckQuorum must be enabled when ReadOnlyOption is ReadOnlyLeaseBased")
	}

	return nil
}

// raft 封装了当前节点所有核心数据
type raft struct {
	id uint64 // 当前节点在集群中的 ID

	Term uint64 // 当前任期号, Term 为 0 表示为本地消息 (即 raft 论文中的 currentTerm)
	Vote uint64 // 当前任期中当前节点将选票投给了哪个节点, (即 raft 论文中的 votedFor)

	readStates []ReadState // todo 只读请求
	readOnly   *readOnly   // 进行批量处理只读请求

	// the log
	raftLog *raftLog // 本地 Log

	maxMsgSize         uint64 // 阈值,单条消息的最大字节数
	maxUncommittedSize uint64
	// TODO(tbg): rename to trk.
	prs tracker.ProgressTracker // 追踪器, 对于 Leader 节点, prs 还负责管理各个 Follower 节点的信息和对应的 NextIndex 和 MatchIndex 索引. 详细介绍可查看 Process 类型 ./raft/tracker/progress.go

	state StateType // 当前节点在集群中的角色

	// isLearner is true if the local raft node is a learner.
	isLearner bool // 标记是否为 Leader 节点

	msgs []pb.Message // 缓存了当前节点等待发送的消息

	// the leader id
	lead uint64 // 当前集群中 Leader 节点的 ID
	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in raft thesis 3.10.
	leadTransferee uint64 // 当发生 Leader 节点转移时,记录转移的目标节点
	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via pendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	pendingConfIndex uint64 // 在本地日志但还没有被应用的日志的索引
	// an estimate of the size of the uncommitted tail of the Raft log. Used to
	// prevent unbounded log growth. Only maintained by the leader. Reset on
	// term changes.
	uncommittedSize uint64

	// number of ticks since it reached last electionTimeout when it is leader
	// or candidate.
	// number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int // 选举计时器的指针,其单位是逻辑时钟的刻度,逻辑时钟每推进一次 ,该字段值就会增加 1 。

	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int // 心跳计时器的指针,其单位也是逻辑时钟的刻度,逻辑时钟每推进一次,该字段值就会增加 1 。

	checkQuorum bool // 每隔一段时间, Leader 节点会尝试连接集群中的其他节点(发送心跳消息),如果发现自己可以连接到节点个数
	// 没有超过半数(即没有收到足够的心跳响应),则主动切换成 Follower 状态。这样在网络分区的场景中,旧的 Leader 节点
	// 可以很快知道自己已经过期,可以减少 Client 连接旧 Leader 节点的等待时间 。

	preVote bool // 为了避免由于网络分区或 Follower 自身网络问题导致的 Follower 节点循环发起选举.当 Follower 节点准备发起一次
	// 选举之前,会先连接集群中的其他节点,并询问它们是否愿意参与选举,如果集群中的其他节点能够正常收到 Leader 节点的心
	// 跳消息,则会拒绝参与选举,反之则参与选举。当在 PreVote 过程中,有超过半数的节点响应并参与新一轮选举,则可以发起新一轮的选举。

	heartbeatTimeout int // 心跳超时时间, 当 heartb eatE l apsed 字段值到达该值时, 就会触发 Leader 节点发送一条心跳消息。
	electionTimeout  int // 选举超时时间,用于计算随机选举超时间
	// randomizedElectionTimeout is a random number between
	// [electiontimeout, 2 * electiontimeout - 1]. It gets reset
	// when raft changes its state to follower or candidate.
	randomizedElectionTimeout int // 当 electionElapsed 宇段值到达该值时,就会触发新一轮的选举。 为了避免同一时刻集群中有多个 Follower
	// 节点发起选举瓜分选票, 所以将各个节点的选举超时时间设置为一个合理的随机数,就可以有效避免这个问题
	disableProposalForwarding bool

	tick func() // 当前节点推进逻辑时钟的函数。如果当前节点是 Leader ,则指向 raft.tickHeart() 函数,如果当前节点是 Follower 或是 Candidate ,则指向 raft. tickElection()函数 。

	step stepFunc // 当前节点收到消息时的处理函数。如果是 Leader 节点, 则该字段指向 stepLeader()函数,如果是 Follower 节点,则该字段指向 stepFollower()函数,
	// 如果是处于 preVote 阶段的节点或是 Candidate 节点,则该字段指向 step Candidate()函数。

	logger Logger
}

func newRaft(c *Config) *raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// 初始化本地日志
	raftlog := newLogWithSize(c.Storage, c.Logger, c.MaxCommittedSizePerReady)

	// Storage 的初始状态是通过本地 Entry 记录回放得到
	hs, cs, err := c.Storage.InitialState()
	if err != nil {
		panic(err) // TODO(bdarnell)
	}

	if len(c.peers) > 0 || len(c.learners) > 0 {
		if len(cs.Voters) > 0 || len(cs.Learners) > 0 {
			// TODO(bdarnell): the peers argument is always nil except in
			// tests; the argument should be removed and these tests should be
			// updated to specify their nodes through a snapshot.
			panic("cannot specify both newRaft(peers, learners) and ConfState.(Voters, Learners)")
		}
		cs.Voters = c.peers
		cs.Learners = c.learners
	}

	r := &raft{
		id:                        c.ID, // 当前节点ID
		lead:                      None, // 当前集群中Leader节点的ID, 初始化为0
		isLearner:                 false,
		raftLog:                   raftlog,
		maxMsgSize:                c.MaxSizePerMsg, // 每条消息的最大字节数, 0 表示每条消息只能带一条 entry, math.MaxUint64 表示没有上限.
		maxUncommittedSize:        c.MaxUncommittedEntriesSize,
		prs:                       tracker.MakeProgressTracker(c.MaxInflightMsgs), // MaxInflightMsgs, 已经发出去但没有收到响应的最大消息个数
		electionTimeout:           c.ElectionTick,                                 // 选举超时时间
		heartbeatTimeout:          c.HeartbeatTick,                                // 心跳超时时间
		logger:                    c.Logger,                                       // 普通日志输出
		checkQuorum:               c.CheckQuorum,                                  // 是否开启 CheckQuorum 模式
		preVote:                   c.PreVote,
		readOnly:                  newReadOnly(c.ReadOnlyOption), // 只读请求的相关配置
		disableProposalForwarding: c.DisableProposalForwarding,
	}

	// TODO:
	cfg, prs, err := confchange.Restore(confchange.Changer{
		Tracker:   r.prs,
		LastIndex: raftlog.lastIndex(),
	}, cs)
	if err != nil {
		panic(err)
	}
	assertConfStatesEquivalent(r.logger, cs, r.switchToConfig(cfg, prs))

	if !IsEmptyHardState(hs) {
		// 根据从 Storage 中获取的 HardState ,初始化 raftLog.committed 字段 ,以及 raft.Term 和 Vote 字段
		r.loadState(hs)
	}
	// 配置 applied, 当前节点已经应用的最后一条 Entry 的索引值,在重启节点的时候传入
	if c.Applied > 0 {
		raftlog.appliedTo(c.Applied)
	}
	// 当前节点切换到 Follower 节点状态
	r.becomeFollower(r.Term, None)

	var nodesStrs []string
	for _, n := range r.prs.VoterNodes() {
		nodesStrs = append(nodesStrs, fmt.Sprintf("%x", n))
	}

	r.logger.Infof("newRaft %x [peers: [%s], term: %d, commit: %d, applied: %d, lastindex: %d, lastterm: %d]",
		r.id, strings.Join(nodesStrs, ","), r.Term, r.raftLog.committed, r.raftLog.applied, r.raftLog.lastIndex(), r.raftLog.lastTerm())
	return r
}

func (r *raft) hasLeader() bool { return r.lead != None }

func (r *raft) softState() *SoftState { return &SoftState{Lead: r.lead, RaftState: r.state} }

func (r *raft) hardState() pb.HardState {
	return pb.HardState{
		Term:   r.Term,
		Vote:   r.Vote,
		Commit: r.raftLog.committed,
	}
}

// send persists state to stable storage and then sends to its mailbox.
func (r *raft) send(m pb.Message) {
	m.From = r.id // 设置消息发送节点的ID, 即当前节点的 ID
	if m.Type == pb.MsgVote || m.Type == pb.MsgVoteResp || m.Type == pb.MsgPreVote || m.Type == pb.MsgPreVoteResp {
		if m.Term == 0 {
			// All {pre-,}campaign messages need to have the term set when
			// sending.
			// - MsgVote: m.Term is the term the node is campaigning for,
			//   non-zero as we increment the term when campaigning.
			// - MsgVoteResp: m.Term is the new r.Term if the MsgVote was
			//   granted, non-zero for the same reason MsgVote is
			// - MsgPreVote: m.Term is the term the node will campaign,
			//   non-zero as we use m.Term to indicate the next term we'll be
			//   campaigning for
			// - MsgPreVoteResp: m.Term is the term received in the original
			//   MsgPreVote if the pre-vote was granted, non-zero for the
			//   same reasons MsgPreVote is
			panic(fmt.Sprintf("term should be set when sending %s", m.Type))
		}
	} else {
		if m.Term != 0 {
			panic(fmt.Sprintf("term should not be set when sending %s (was %d)", m.Type, m.Term))
		}
		// do not attach term to MsgProp, MsgReadIndex
		// proposals are a way to forward to the leader and
		// should be treated as local message.
		// MsgReadIndex is also forwarded to leader.
		if m.Type != pb.MsgProp && m.Type != pb.MsgReadIndex {
			m.Term = r.Term
		}
	}
	// 将消息添加到 r.msgs 切片中等待发送
	r.msgs = append(r.msgs, m)
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer.
func (r *raft) sendAppend(to uint64) {
	r.maybeSendAppend(to, true)
}

// maybeSendAppend sends an append RPC with new entries to the given peer,
// if necessary. Returns true if a message was sent. The sendIfEmpty
// argument controls whether messages with no entries will be sent
// ("empty" messages are useful to convey updated Commit indexes, but
// are undesirable when we're sending multiple messages in a batch).
func (r *raft) maybeSendAppend(to uint64, sendIfEmpty bool) bool {
	pr := r.prs.Progress[to]
	if pr.IsPaused() { // 检测当前节点是否可以向目标节点发送消息
		return false
	}
	m := pb.Message{} // 创建待发送的消息
	m.To = to         // 设置目标节点的 ID

	// 查找待发送的消息，当前 Leader 节点根据每个目标节点的 Next 索引， 发送指定的 entries 记录
	term, errt := r.raftLog.term(pr.Next - 1)
	ents, erre := r.raftLog.entries(pr.Next, r.maxMsgSize)
	if len(ents) == 0 && !sendIfEmpty {
		return false
	}

	// 如果查找目标节点对应的 term 或 entries 失败， 就发送快照消息 MsgSnap
	if errt != nil || erre != nil { // send snapshot if we failed to get term or entries
		if !pr.RecentActive {
			// 如果 Leader 发现该 Follower 已经不再存活状态，就不会进行任何处理
			r.logger.Debugf("ignore sending snapshot to %x since it is not recently active", to)
			return false
		}

		m.Type = pb.MsgSnap
		snapshot, err := r.raftLog.snapshot() // 获取快照数据
		if err != nil {
			if err == ErrSnapshotTemporarilyUnavailable {
				r.logger.Debugf("%x failed to send snapshot to %x because snapshot is temporarily unavailable", r.id, to)
				return false
			}
			panic(err) // TODO(bdarnell)
		}
		if IsEmptySnap(snapshot) {
			panic("need non-empty snapshot")
		}
		m.Snapshot = snapshot
		sindex, sterm := snapshot.Metadata.Index, snapshot.Metadata.Term
		r.logger.Debugf("%x [firstindex: %d, commit: %d] sent snapshot[index: %d, term: %d] to %x [%s]",
			r.id, r.raftLog.firstIndex(), r.raftLog.committed, sindex, sterm, to, pr)
		// 将目标 Follower 节点对应的 Process 切换成 ProcessStageSnapshot 状态， 用 Process.PendingSnapshot 字段记录快照数据的信息
		pr.BecomeSnapshot(sindex)
		r.logger.Debugf("%x paused sending replication messages to %x [%s]", r.id, to, pr)
	} else {
		m.Type = pb.MsgApp
		m.Index = pr.Next - 1 // 表示其携带的 entries 中前一条记录的索引值
		m.LogTerm = term
		m.Entries = ents
		m.Commit = r.raftLog.committed // 消息发送节点的提交位置，当前节点最后一条已提交的记录索引值
		if n := len(m.Entries); n != 0 {
			// 根据目标节点对应的 Progress.State 状态决定其发送消息后的行为
			switch pr.State {
			// optimistically increase the next when in StateReplicate
			case tracker.StateReplicate:
				last := m.Entries[n-1].Index
				pr.OptimisticUpdate(last) // 更新目标节点的 Next 值， 这里不会更新 Match
				pr.Inflights.Add(last)    // 记录已发送但是未收到响应的消息
			case tracker.StateProbe:
				pr.ProbeSent = true // 消息发送后，就将 ProbeSent 设置为 true， 暂停后续消息的发送
			default:
				r.logger.Panicf("%x is sending append in unhandled state %s", r.id, pr.State)
			}
		}
	}

	// 将消息添加到 raft.msgs 中等待发送
	r.send(m)
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *raft) sendHeartbeat(to uint64, ctx []byte) {
	// Attach the commit as min(to.matched, r.committed).
	// When the leader sends out heartbeat message,
	// the receiver(follower) might not be matched with the leader
	// or it might not have all the committed entries.
	// The leader MUST NOT forward the follower's commit to
	// an unmatched index.
	// 在发送该 MsgHeartbeat 消息时，Follower 节点不一定已经收到了全部已提交的 Entry 记录
	commit := min(r.prs.Progress[to].Match, r.raftLog.committed)
	m := pb.Message{
		To:      to,
		Type:    pb.MsgHeartbeat,
		Commit:  commit,
		Context: ctx,
	}

	r.send(m)
}

// bcastAppend sends RPC, with entries to all peers that are not up-to-date
// according to the progress recorded in r.prs.
func (r *raft) bcastAppend() {
	r.prs.Visit(func(id uint64, _ *tracker.Progress) {
		if id == r.id {
			return
		}
		r.sendAppend(id)
	})
}

// bcastHeartbeat sends RPC, without entries to all the peers.
func (r *raft) bcastHeartbeat() {
	lastCtx := r.readOnly.lastPendingRequestCtx()
	if len(lastCtx) == 0 {
		r.bcastHeartbeatWithCtx(nil)
	} else {
		r.bcastHeartbeatWithCtx([]byte(lastCtx))
	}
}

func (r *raft) bcastHeartbeatWithCtx(ctx []byte) {
	r.prs.Visit(func(id uint64, _ *tracker.Progress) {
		if id == r.id {
			return
		}
		r.sendHeartbeat(id, ctx)
	})
}

func (r *raft) advance(rd Ready) {
	// If entries were applied (or a snapshot), update our cursor for
	// the next Ready. Note that if the current HardState contains a
	// new Commit index, this does not mean that we're also applying
	// all of the new entries due to commit pagination by size.
	if index := rd.appliedCursor(); index > 0 {
		r.raftLog.appliedTo(index)
		if r.prs.Config.AutoLeave && index >= r.pendingConfIndex && r.state == StateLeader {
			// If the current (and most recent, at least for this leader's term)
			// configuration should be auto-left, initiate that now.
			ccdata, err := (&pb.ConfChangeV2{}).Marshal()
			if err != nil {
				panic(err)
			}
			ent := pb.Entry{
				Type: pb.EntryConfChangeV2,
				Data: ccdata,
			}
			if !r.appendEntry(ent) {
				// If we could not append the entry, bump the pending conf index
				// so that we'll try again later.
				//
				// TODO(tbg): test this case.
				r.pendingConfIndex = r.raftLog.lastIndex()
			} else {
				r.logger.Infof("initiating automatic transition out of joint configuration %s", r.prs.Config)
			}
		}
	}
	r.reduceUncommittedSize(rd.CommittedEntries)

	if len(rd.Entries) > 0 {
		e := rd.Entries[len(rd.Entries)-1]
		r.raftLog.stableTo(e.Index, e.Term)
	}
	if !IsEmptySnap(rd.Snapshot) {
		r.raftLog.stableSnapTo(rd.Snapshot.Metadata.Index)
	}
}

// maybeCommit attempts to advance the commit index. Returns true if
// the commit index changed (in which case the caller should call
// r.bcastAppend).
// 除了 appendEntry() 方法,在 Leader 节点每次收到 MsgAppResp 消息时也会调用 maybeCommit()方法
func (r *raft) maybeCommit() bool {
	mci := r.prs.Committed() // 根据投票情况,返回已知的要提交的最大的索引
	return r.raftLog.maybeCommit(mci, r.Term)
}

// 在节点借还状态时, 重置 raft 实例的多个字段
func (r *raft) reset(term uint64) {
	if r.Term != term {
		r.Term = term // 重置任期 Term
		r.Vote = None // 清空投票对象 VoteFor
	}
	r.lead = None // 清空集群 Leader 节点

	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.resetRandomizedElectionTimeout() // 重置选举超时时间 随机值

	r.abortLeaderTransfer() // 清空 leadTransferee, Leader 发生转移的时候的目标节点.

	r.prs.ResetVotes() // 重置 votes 字段
	// 重置 prs , 其中每个 Progress 中的 Next 设置为 raftLog.lastindex + 1
	r.prs.Visit(func(id uint64, pr *tracker.Progress) {
		*pr = tracker.Progress{
			Match:     0,
			Next:      r.raftLog.lastIndex() + 1,
			Inflights: tracker.NewInflights(r.prs.MaxInflight),
			IsLearner: pr.IsLearner,
		}
		if id == r.id {
			pr.Match = r.raftLog.lastIndex()
		}
	})

	r.pendingConfIndex = 0
	r.uncommittedSize = 0
	r.readOnly = newReadOnly(r.readOnly.option)
}

func (r *raft) appendEntry(es ...pb.Entry) (accepted bool) {
	li := r.raftLog.lastIndex() // 获取本地日志中最后一条日志
	// 更新待追加的 Entry 的 index 和 Term
	for i := range es {
		es[i].Term = r.Term
		es[i].Index = li + 1 + uint64(i)
	}
	// Track the size of this uncommitted proposal.
	if !r.increaseUncommittedSize(es) {
		r.logger.Debugf(
			"%x appending new entries to log would exceed uncommitted entry size limit; dropping proposal",
			r.id,
		)
		// Drop the proposal.
		return false
	}
	// use latest "last" index after truncate/append
	li = r.raftLog.append(es...) // 向本地日志追加 entries
	// 更新当前节点对应的 Progress , 主要是更新 Next 和 Match
	r.prs.Progress[r.id].MaybeUpdate(li)
	// Regardless of maybeCommit's return, our caller will call bcastAppend.
	r.maybeCommit() // 尝试提交 Entry 记录
	return true
}

// tickElection is run by followers and candidates after r.electionTimeout.
// Follower 节点周期性地调用 raft.tickElection()方法推进 electionElapsed 并检测是否超时
func (r *raft) tickElection() {
	r.electionElapsed++

	// promotable () 方法会检查 prs 字段中是否还存在当前节点对应的 Progress 实例 ,这是为了检测当前节点是否被从集群中移除了
	// pastElectionTimeout() 方法是检测当前的选举计时器是否超时( r.electionElapsed >= r.randomizedElectionTimeout )
	if r.promotable() && r.pastElectionTimeout() {
		r.electionElapsed = 0
		r.Step(pb.Message{From: r.id, Type: pb.MsgHup}) // 发起选举
	}
}

// tickHeartbeat is run by leaders to send a MsgBeat after r.heartbeatTimeout.
// leader 向其他节点发送心跳消息
func (r *raft) tickHeartbeat() {
	r.heartbeatElapsed++ // 递增心跳计时器
	r.electionElapsed++  // 递增选举计时器

	if r.electionElapsed >= r.electionTimeout {
		r.electionElapsed = 0 // 重置选举计时器，leader节点不会主动发起选举
		// 进行多数检测
		if r.checkQuorum {
			// 当选举计时器超过 electionTimeout 时，会触发一次 checkQuorum 操作， 该操作并不会发送网络消息， 只是检测当前节点是否与大部分节点连通
			r.Step(pb.Message{From: r.id, Type: pb.MsgCheckQuorum})
		}
		// If current leader cannot transfer leadership in electionTimeout, it becomes leader again.
		// 选举计时器处于 electionTimeout ~ randomizedElectionTimeout 时， 不能进行 Leader 节点的转移
		if r.state == StateLeader && r.leadTransferee != None {
			r.abortLeaderTransfer() // 清空 raft.leadTransferee 字段， 放弃转移
		}
	}

	if r.state != StateLeader {
		return
	}

	// 心跳计时超时
	if r.heartbeatElapsed >= r.heartbeatTimeout {
		r.heartbeatElapsed = 0                           // 重置心跳计时器
		r.Step(pb.Message{From: r.id, Type: pb.MsgBeat}) // 发送 MsgBeat
	}
}

func (r *raft) becomeFollower(term uint64, lead uint64) {
	r.step = stepFollower   // stepFollower 实现了 Follower 处理各类消息的行为
	r.reset(term)           // 重置 raft 实例的 Term,Vote 等字段.
	r.tick = r.tickElection // 当前节点推进逻辑时钟的函数。如果当前节点是 Leader ,则指向 raft.tickHeart() 函数,如果当前节点是 Follower 或是 Candidate ,则指向 raft. tickElection()函数 。
	r.lead = lead           // 设置当前集群的 Leader 节点
	r.state = StateFollower // 设置当前节点状态
	r.logger.Infof("%x became follower at term %d", r.id, r.Term)
}

func (r *raft) becomeCandidate() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateLeader {
		panic("invalid transition [leader -> candidate]")
	}
	r.step = stepCandidate
	r.reset(r.Term + 1) // 重点, 成为候选人后,首先要将当前节点的任期 Term 加 1
	r.tick = r.tickElection
	r.Vote = r.id // 重点, 成为候选人后,将票投给自己.
	r.state = StateCandidate
	r.logger.Infof("%x became candidate at term %d", r.id, r.Term)
}

// 如果开启了 PreVote 模式,当 Follower 节点的选举计时器超时后,会先调用 becomePreCandidate() 将当前状态切换到 PreCandidate 的状态,
// 在 PreCandidate 状态下的节点会先确定自己是否可以与其他节点通信,以此决定自己是否可以转换为 Candidate 状态,
// 这一步是 Raft 为了避免网络分区造成的频繁选举.
func (r *raft) becomePreCandidate() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateLeader {
		// 禁止直接从 Leader 状态切换到 PreCandidate 状态(
		panic("invalid transition [leader -> pre-candidate]")
	}
	// Becoming a pre-candidate changes our step functions and state,
	// but doesn't change anything else. In particular it does not increase
	// r.Term or change r.Vote.
	r.step = stepCandidate
	r.prs.ResetVotes()
	r.tick = r.tickElection
	r.lead = None
	r.state = StatePreCandidate
	r.logger.Infof("%x became pre-candidate at term %d", r.id, r.Term)
}

func (r *raft) becomeLeader() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateFollower {
		panic("invalid transition [follower -> leader]")
	}
	r.step = stepLeader
	r.reset(r.Term)
	r.tick = r.tickHeartbeat
	r.lead = r.id
	r.state = StateLeader

	// Followers enter replicate mode when they've been successfully probed
	// (perhaps after having received a snapshot as a result). The leader is
	// trivially in this state. Note that r.reset() has initialized this
	// progress with the last index already.
	r.prs.Progress[r.id].BecomeReplicate()

	// Conservatively set the pendingConfIndex to the last index in the
	// log. There may or may not be a pending config change, but it's
	// safe to delay any future proposals until we commit all our
	// pending log entries, and scanning the entire tail of the log
	// could be expensive.
	r.pendingConfIndex = r.raftLog.lastIndex()

	emptyEnt := pb.Entry{Data: nil}
	if !r.appendEntry(emptyEnt) {
		// This won't happen because we just called reset() above.
		r.logger.Panic("empty entry was dropped")
	}
	// As a special case, don't count the initial empty entry towards the
	// uncommitted log quota. This is because we want to preserve the
	// behavior of allowing one entry larger than quota if the current
	// usage is zero.
	r.reduceUncommittedSize([]pb.Entry{emptyEnt})
	r.logger.Infof("%x became leader at term %d", r.id, r.Term)
}

// campaign transitions the raft instance to candidate state. This must only be called after verifying that this is a legitimate transition.
// 除了完成状态的切换,还会向集群中的其他节点发送相应类型的消息, 但在调用前,最好验证这是不是一个合法的调用
func (r *raft) campaign(t CampaignType) {
	if !r.promotable() {
		// 这个检查应该在调用这个函数之前就进行
		// This path should not be hit (callers are supposed to check), but better safe than sorry.
		r.logger.Warningf("%x is unpromotable; campaign() should have been called", r.id)
	}
	var term uint64
	var voteMsg pb.MessageType
	if t == campaignPreElection {
		r.becomePreCandidate()
		voteMsg = pb.MsgPreVote
		// PreVote RPCs are sent for the next term before we've incremented r.Term.
		// 在为 r.Term 加 1 前, PreVote 请求应该先 加 1
		term = r.Term + 1
	} else {
		r.becomeCandidate()
		voteMsg = pb.MsgVote
		term = r.Term
	}
	// 判断是否可以通过选举
	if _, _, res := r.poll(r.id, voteRespMsgType(voteMsg), true); res == quorum.VoteWon {
		// We won the election after voting for ourselves (which must mean that
		// this is a single-node cluster). Advance to the next state.
		if t == campaignPreElection {
			// preVote 通过, 可以将当前节点转换为 Candidate 类型
			r.campaign(campaignElection)
		} else {
			r.becomeLeader()
		}
		return
	}

	// 还无法判断为通过选举
	var ids []uint64
	{
		idMap := r.prs.Voters.IDs()
		ids = make([]uint64, 0, len(idMap))
		for id := range idMap {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	for _, id := range ids {
		if id == r.id { // 跳过当前节点自身
			continue
		}
		r.logger.Infof("%x [logterm: %d, index: %d] sent %s request to %x at term %d",
			r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), voteMsg, id, r.Term)

		var ctx []byte
		// 在进行 Leader 节点转移时, MsgPreVote 或 MsgVote 消息会在 Context 中设置该特殊值
		if t == campaignTransfer {
			ctx = []byte(t)
		}
		// 发送指定类型的消息,其中 Index 和 LogTerm 分别是当前节点的 raftLog 中的最后一条消息的 Index 和 Term 值
		r.send(pb.Message{Term: term, To: id, Type: voteMsg, Index: r.raftLog.lastIndex(), LogTerm: r.raftLog.lastTerm(), Context: ctx})
	}
}

// 计算选举结果
func (r *raft) poll(fromID uint64, t pb.MessageType, v bool) (granted int, rejected int, result quorum.VoteResult) {
	// v 是否是拒绝
	if v {
		r.logger.Infof("%x received %s from %x at term %d", r.id, t, fromID, r.Term)
	} else {
		r.logger.Infof("%x received %s rejection from %x at term %d", r.id, t, fromID, r.Term)
	}
	r.prs.RecordVote(fromID, v)
	return r.prs.TallyVotes()
}

// Step 方法是 raft 模块处理消息的入口,代码比较长,主要可以分为两部分, 第一部分是根据 Term 的值对消息进行分类,第二部分是根据消息的类型进行相应的处理
func (r *raft) Step(m pb.Message) error {
	// Handle the message term, which may result in our stepping down to a follower.
	switch {
	case m.Term == 0:
		// local message 本地消息
	case m.Term > r.Term:
		// 在发起选举的时候, 其他节点收到选举请求的时候会走到这个， m.Term 一般会比 r.Term 大 1
		if m.Type == pb.MsgVote || m.Type == pb.MsgPreVote {
			// 根据消息的 Context 判断消息类型是否是 Leader 节点转移场景下产生的，如果是，则强制当前节点参与本次选举或预选
			force := bytes.Equal(m.Context, []byte(campaignTransfer))
			// 释放： 满足这一系列条件，此节点就参与选举
			inLease := r.checkQuorum && r.lead != None && r.electionElapsed < r.electionTimeout
			if !force && inLease {
				// 当前节点不参与此次选举
				// If a server receives a RequestVote request within the minimum election timeout
				// of hearing from a current leader, it does not update its term or grant its vote
				r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] ignored %s from %x [logterm: %d, index: %d] at term %d: lease is not expired (remaining ticks: %d)",
					r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term, r.electionTimeout-r.electionElapsed)
				return nil
			}
		}
		switch {
		case m.Type == pb.MsgPreVote:
			// Never change our term in response to a PreVote
		case m.Type == pb.MsgPreVoteResp && !m.Reject:
			// We send pre-vote requests with a term in our future. If the
			// pre-vote is granted, we will increment our term when we get a
			// quorum. If it is not, the term comes from the node that
			// rejected our vote so we should become a follower at the new
			// term.
		default:
			r.logger.Infof("%x [term: %d] received a %s message with higher term from %x [term: %d]",
				r.id, r.Term, m.Type, m.From, m.Term)
			if m.Type == pb.MsgApp || m.Type == pb.MsgHeartbeat || m.Type == pb.MsgSnap {
				r.becomeFollower(m.Term, m.From)
			} else {
				r.becomeFollower(m.Term, None)
			}
		}

	case m.Term < r.Term:
		if (r.checkQuorum || r.preVote) && (m.Type == pb.MsgHeartbeat || m.Type == pb.MsgApp) {
			// We have received messages from a leader at a lower term. It is possible
			// that these messages were simply delayed in the network, but this could
			// also mean that this node has advanced its term number during a network
			// partition, and it is now unable to either win an election or to rejoin
			// the majority on the old term. If checkQuorum is false, this will be
			// handled by incrementing term numbers in response to MsgVote with a
			// higher term, but if checkQuorum is true we may not advance the term on
			// MsgVote and must generate other messages to advance the term. The net
			// result of these two features is to minimize the disruption caused by
			// nodes that have been removed from the cluster's configuration: a
			// removed node will send MsgVotes (or MsgPreVotes) which will be ignored,
			// but it will not receive MsgApp or MsgHeartbeat, so it will not create
			// disruptive term increases, by notifying leader of this node's activeness.
			// The above comments also true for Pre-Vote
			//
			// When follower gets isolated, it soon starts an election ending
			// up with a higher term than leader, although it won't receive enough
			// votes to win the election. When it regains connectivity, this response
			// with "pb.MsgAppResp" of higher term would force leader to step down.
			// However, this disruption is inevitable to free this stuck node with
			// fresh election. This can be prevented with Pre-Vote phase.
			r.send(pb.Message{To: m.From, Type: pb.MsgAppResp})
		} else if m.Type == pb.MsgPreVote {
			// Before Pre-Vote enable, there may have candidate with higher term,
			// but less log. After update to Pre-Vote, the cluster may deadlock if
			// we drop messages with a lower term.
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] rejected %s from %x [logterm: %d, index: %d] at term %d",
				r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term)
			r.send(pb.Message{To: m.From, Term: r.Term, Type: pb.MsgPreVoteResp, Reject: true})
		} else {
			// ignore other cases
			r.logger.Infof("%x [term: %d] ignored a %s message with lower term from %x [term: %d]",
				r.id, r.Term, m.Type, m.From, m.Term)
		}
		return nil
	}

	switch m.Type {
	case pb.MsgHup: // 本地消息, 发起选举
		if r.state != StateLeader { // 只有非 Leader 节点才会处理 MsgHup 类型的消息
			if !r.promotable() { // 检测当前节点能够被选举为 Leader 节点
				r.logger.Warningf("%x is unpromotable and can not campaign; ignoring MsgHup", r.id)
				return nil
			}
			// 获取本地日志中已提交但未应用的日志
			ents, err := r.raftLog.slice(r.raftLog.applied+1, r.raftLog.committed+1, noLimit)
			if err != nil {
				r.logger.Panicf("unexpected error getting unapplied entries (%v)", err)
			}

			// 检测是否有未应用的 EntryConfChange 记录,如果有就放弃发起选举的机会
			if n := numOfPendingConf(ents); n != 0 && r.raftLog.committed > r.raftLog.applied {
				r.logger.Warningf("%x cannot campaign at term %d since there are still %d pending configuration changes to apply", r.id, r.Term, n)
				return nil
			}

			r.logger.Infof("%x is starting a new election at term %d", r.id, r.Term)
			// 调用 campaign 方法切换节点状态
			if r.preVote {
				r.campaign(campaignPreElection)
			} else {
				r.campaign(campaignElection)
			}
		} else {
			r.logger.Debugf("%x ignoring MsgHup because already leader", r.id)
		}

	case pb.MsgVote, pb.MsgPreVote: // 本地消息 MsgHup 经过处理，最终会向其他节点发送 MsgVote 或 MsgPreVote 消息
		// 当前节点在参与选举或预选时，会综合下面几个条件决定是否投票（这些在 raft 协议中可以找到对应的描述）：
		// 1. 当前节点是否已经投过票
		// 2. MsgPreVote 消息发送者的任期号是否更大
		// 3. 当前节点投票给了对方节点
		// 4. MsgPreVote 消息的发送者的 raftLog 中是否包含当前节点的全部 Entry 记录 （isUpToDate ）

		// We can vote if this is a repeat of a vote we've already cast... 可以重复投票
		canVote := r.Vote == m.From ||
			// ...we haven't voted and we don't think there's a leader yet in this term...
			(r.Vote == None && r.lead == None) ||
			// ...or this is a PreVote for a future term...
			(m.Type == pb.MsgPreVote && m.Term > r.Term)
		// ...and we believe the candidate is up to date.
		if canVote && r.raftLog.isUpToDate(m.Index, m.LogTerm) {
			// Note: it turns out that that learners must be allowed to cast votes.
			// This seems counter- intuitive but is necessary in the situation in which
			// a learner has been promoted (i.e. is now a voter) but has not learned
			// about this yet.
			// For example, consider a group in which id=1 is a learner and id=2 and
			// id=3 are voters. A configuration change promoting 1 can be committed on
			// the quorum `{2,3}` without the config change being appended to the
			// learner's log. If the leader (say 2) fails, there are de facto two
			// voters remaining. Only 3 can win an election (due to its log containing
			// all committed entries), but to do so it will need 1 to vote. But 1
			// considers itself a learner and will continue to do so until 3 has
			// stepped up as leader, replicates the conf change to 1, and 1 applies it.
			// Ultimately, by receiving a request to vote, the learner realizes that
			// the candidate believes it to be a voter, and that it should act
			// accordingly. The candidate's config may be stale, too; but in that case
			// it won't win the election, at least in the absence of the bug discussed
			// in:
			// https://github.com/etcd-io/etcd/issues/7625#issuecomment-488798263.
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] cast %s for %x [logterm: %d, index: %d] at term %d",
				r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term)
			// When responding to Msg{Pre,}Vote messages we include the term
			// from the message, not the local term. To see why, consider the
			// case where a single node was previously partitioned away and
			// it's local term is now out of date. If we include the local term
			// (recall that for pre-votes we don't update the local term), the
			// (pre-)campaigning node on the other end will proceed to ignore
			// the message (it ignores all out of date messages).
			// The term in the original message and current local term are the
			// same in the case of regular votes, but different for pre-votes.
			// 返回成功票
			r.send(pb.Message{To: m.From, Term: m.Term, Type: voteRespMsgType(m.Type)})
			if m.Type == pb.MsgVote {
				// Only record real votes.
				r.electionElapsed = 0
				r.Vote = m.From
			}
		} else {
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] rejected %s from %x [logterm: %d, index: %d] at term %d",
				r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term)
			// 返回拒绝票
			r.send(pb.Message{To: m.From, Term: r.Term, Type: voteRespMsgType(m.Type), Reject: true})
		}

	default: // 其他类型的消息就交给当前节点的 step 处理函数处理
		err := r.step(r, m)
		if err != nil {
			return err
		}
	}
	return nil
}

type stepFunc func(r *raft, m pb.Message) error

func stepLeader(r *raft, m pb.Message) error {
	// These message types do not require any progress for m.From.
	// 此类消息无须处理消息的 From 字段和消息发送者对应的 Process 实例可以直接进行处理
	switch m.Type {
	case pb.MsgBeat: // 本地消息，向所有节点发送细腻套
		r.bcastHeartbeat()
		return nil
	case pb.MsgCheckQuorum:
		// The leader should always see itself as active. As a precaution, handle
		// the case in which the leader isn't in the configuration any more (for
		// example if it just removed itself).
		//
		// TODO(tbg): I added a TODO in removeNode, it doesn't seem that the
		// leader steps down when removing itself. I might be missing something.
		if pr := r.prs.Progress[r.id]; pr != nil {
			pr.RecentActive = true
		}
		// 检测当前 Leader 节点是否与集群中大部分节点连通，如果不连通，则切换成 Follower 状态
		if !r.prs.QuorumActive() {
			r.logger.Warningf("%x stepped down to follower since quorum is not active", r.id)
			r.becomeFollower(r.Term, None)
		}
		// Mark everyone (but ourselves) as inactive in preparation for the next
		// CheckQuorum.
		r.prs.Visit(func(id uint64, pr *tracker.Progress) {
			if id != r.id {
				pr.RecentActive = false
			}
		})
		return nil
	case pb.MsgProp:
		if len(m.Entries) == 0 {
			r.logger.Panicf("%x stepped empty MsgProp", r.id)
		}

		// 检测当前节点是否被移除集群， 若被移除，则并不对消息进行处理
		if r.prs.Progress[r.id] == nil {
			// If we are not currently a member of the range (i.e. this node
			// was removed from the configuration while serving as leader),
			// drop any new proposals.
			return ErrProposalDropped
		}

		// 检测当前是否正在进行 Leader 转移，若在进行则不处理消息
		if r.leadTransferee != None {
			r.logger.Debugf("%x [term %d] transfer leadership to %x is in progress; dropping proposal", r.id, r.Term, r.leadTransferee)
			return ErrProposalDropped
		}

		// 遍历写请求消息中携带的全部 Entry 记录
		for i := range m.Entries {
			e := &m.Entries[i]
			var cc pb.ConfChangeI
			if e.Type == pb.EntryConfChange {
				var ccc pb.ConfChange
				if err := ccc.Unmarshal(e.Data); err != nil {
					panic(err)
				}
				cc = ccc
			} else if e.Type == pb.EntryConfChangeV2 {
				var ccc pb.ConfChangeV2
				if err := ccc.Unmarshal(e.Data); err != nil {
					panic(err)
				}
				cc = ccc
			}
			if cc != nil {
				alreadyPending := r.pendingConfIndex > r.raftLog.applied
				alreadyJoint := len(r.prs.Config.Voters[1]) > 0
				wantsLeaveJoint := len(cc.AsV2().Changes) == 0

				var refused string
				if alreadyPending {
					refused = fmt.Sprintf("possible unapplied conf change at index %d (applied to %d)", r.pendingConfIndex, r.raftLog.applied)
				} else if alreadyJoint && !wantsLeaveJoint {
					refused = "must transition out of joint config first"
				} else if !alreadyJoint && wantsLeaveJoint {
					refused = "not in joint state; refusing empty conf change"
				}

				if refused != "" {
					r.logger.Infof("%x ignoring conf change %v at config %s: %s", r.id, cc, r.prs.Config, refused)
					m.Entries[i] = pb.Entry{Type: pb.EntryNormal}
				} else {
					r.pendingConfIndex = r.raftLog.lastIndex() + uint64(i) + 1
				}
			}
		}

		if !r.appendEntry(m.Entries...) {
			return ErrProposalDropped
		}
		r.bcastAppend()
		return nil
	case pb.MsgReadIndex:
		// If more than the local vote is needed, go through a full broadcast, otherwise optimize.
		if !r.prs.IsSingleton() { // 不是单节点，表示集群环境
			// Leader 节点检测自身在当前任期中是否已提交过 Entry 记录， 如果没有，则无法进行读取操作
			if r.raftLog.zeroTermOnErrCompacted(r.raftLog.term(r.raftLog.committed)) != r.Term {
				// Reject read only request when this leader has not committed any log entry at its term.
				return nil
			}

			// thinking: use an interally defined context instead of the user given context.
			// We can express this in terms of the term and index instead of a user-supplied value.
			// This would allow multiple reads to piggyback on the same message.
			//
			switch r.readOnly.option {
			case ReadOnlySafe:
				// ReadOnlySafe 是推荐的模式，只读请求不会受节点之间时钟差异和网络分区的影响
				// 记录当前节点的 raftLog.committed（已提交的位置） 字段值, 和消息相关的信息
				r.readOnly.addRequest(r.raftLog.committed, m)
				// The local node automatically acks the request.
				r.readOnly.recvAck(r.id, m.Entries[0].Data)
				r.bcastHeartbeatWithCtx(m.Entries[0].Data) // 向其他集群发送发送 MsgHeartbeat 心跳 消息。
			case ReadOnlyLeaseBased:
				ri := r.raftLog.committed
				if m.From == None || m.From == r.id { // from local member
					r.readStates = append(r.readStates, ReadState{Index: ri, RequestCtx: m.Entries[0].Data})
				} else {
					r.send(pb.Message{To: m.From, Type: pb.MsgReadIndexResp, Index: ri, Entries: m.Entries})
				}
			}
		} else {                                  // 单节点 only one voting member (the leader) in the cluster
			if m.From == None || m.From == r.id { // from leader itself
				r.readStates = append(r.readStates, ReadState{Index: r.raftLog.committed, RequestCtx: m.Entries[0].Data})
			} else { // from learner member
				r.send(pb.Message{To: m.From, Type: pb.MsgReadIndexResp, Index: r.raftLog.committed, Entries: m.Entries})
			}
		}

		return nil
	}

	// All other message types require a progress for m.From (pr).
	pr := r.prs.Progress[m.From]
	if pr == nil { // 对应的 Process 实例可能已经从集群中删除
		r.logger.Debugf("%x no progress available for %x", r.id, m.From)
		return nil
	}
	switch m.Type {
	case pb.MsgAppResp:
		pr.RecentActive = true // Leader 节点接收到 MsgAppResp 消息，表示发送方在正常运行

		if m.Reject { // MsgApp 请求被拒绝， 根据 MsgAppResp 携带的 Index 信心，重新设置 Leader 节点上对应的该发送发的 nextIndex 值
			r.logger.Debugf("%x received MsgAppResp(MsgApp was rejected, lastindex: %d) from %x for index %d",
				r.id, m.RejectHint, m.From, m.Index)
			if pr.MaybeDecrTo(m.Index, m.RejectHint) {
				r.logger.Debugf("%x decreased progress of %x to [%s]", r.id, m.From, pr)
				if pr.State == tracker.StateReplicate {
					// MsgApp 消息被拒后，会将发送方节点的状态设置为 StateProbe， 表示 Leader 节点一次只能向该节点发送一条消息，等收到响应后才能发送下一条
					// 试探 Follower 节点的匹配位置
					pr.BecomeProbe()
				}
				// 再次向对应的 Follower 节点发送 MsgApp 消息，在 sendAppend() 方法中，会将对应的 Process.pause 字段设置为 true， 从而暂停后续消息的发送，实现向 Follower 节点发送试探性消息的效果
				r.sendAppend(m.From)
			}
		} else { // 表示之前发送的 MsgApp 消息被对应的 Follower 节点接收成功
			oldPaused := pr.IsPaused()
			// m.Index 对应的是 Follower 节点本地日志 raftLog 中最后一条 Entry 记录的索引，这里会根据改值更新
			// 其在 Leader 节点对应的 Process 实例的 Next 和 Match 值
			if pr.MaybeUpdate(m.Index) {
				switch {
				case pr.State == tracker.StateProbe:
					// 一旦 MsgApp 被 Follower 节点接收，则表示已经找到其正确的 Next 和 Match， 不必再进行试探，可以将对应的 Process.state 更新为 StateReplicate
					pr.BecomeReplicate()
				case pr.State == tracker.StateSnapshot && pr.Match >= pr.PendingSnapshot:
					// 之前由于某些原因，Leader 节点通过发送快照的方式恢复 Follower 节点，但在发送 MsgSnap 消息的过程中，Follower 节点恢复并正常接受了 Leader 的 MsgApp 消息
					// 此时会丢弃 MsgSnap 消息，并开始尝试试探该 Follower 节点正确的 Match 和 Next 值
					// TODO(tbg): we should also enter this branch if a snapshot is
					// received that is below pr.PendingSnapshot but which makes it
					// possible to use the log again.
					r.logger.Debugf("%x recovered from needing snapshot, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
					// Transition back to replicating state via probing state
					// (which takes the snapshot into account). If we didn't
					// move to replicating state, that would only happen with
					// the next round of appends (but there may not be a next
					// round for a while, exposing an inconsistent RaftStatus).
					// TODO question: 为什么这里先 BecomeProbe() 又 BecomeReplicate()
					pr.BecomeProbe()
					pr.BecomeReplicate()
				case pr.State == tracker.StateReplicate:
					// 之前向某个 Follower 节点发送 MsgApp 消息时，会将其相关信息保存到对应的 Progress.Inflights 中， 这里收到成功的 MsgAppResp 响应后，
					// 会将其对应的索引中 Inflights 中删除，这样就实现了限流的效果，避免网络出现延迟时，继续发送消息，从而导致网络拥堵
					pr.Inflights.FreeLE(m.Index)
				}

				// 收到一个 MsgAppResp 后，还会尝试更新本地日志的已提交索引 raft.committed.
				// 因为有些 Entry 记录可能已经被保存到了半数以上的节点中
				if r.maybeCommit() {
					// 向所有节点广播 MsgApp 消息， 此时 Commit 字段与上一次 MsgApp 消息已经不同
					r.bcastAppend()
				} else if oldPaused {
					// 如果之前 Leader 节点暂停向该 Follower 节点发送消息，收到 MsgAppResp 消息后，
					// 在上述代码中已经重置了相应的状态，所以可以继续发送 MsgApp 消息
					// If we were paused before, this node may be missing the
					// latest commit index, so send it.
					r.sendAppend(m.From)
				}
				// We've updated flow control information above, which may
				// allow us to send multiple (size-limited) in-flight messages
				// at once (such as when transitioning from probe to
				// replicate, or when freeTo() covers multiple messages). If
				// we have more entries to send, send as many messages as we
				// can (without sending empty messages for the commit index)
				for r.maybeSendAppend(m.From, false) {
				}
				// Transfer leadership is in progress.
				if m.From == r.leadTransferee && pr.Match == r.raftLog.lastIndex() {
					r.logger.Infof("%x sent MsgTimeoutNow to %x after received MsgAppResp", r.id, m.From)
					r.sendTimeoutNow(m.From)
				}
			}
		}
	case pb.MsgHeartbeatResp:
		pr.RecentActive = true // 更新消息来源 Follower 节点对应的 RecentActive 值， 表示该 Follower 节点与 Leader 节点正常连通
		pr.ProbeSent = false   //

		// free one slot for the full inflights window to allow progress.
		if pr.State == tracker.StateReplicate && pr.Inflights.Full() {
			// 释放第一个消息，这样就可以开始后续消息的发送
			pr.Inflights.FreeFirstOne()
		}

		// 当 Leader 节点收到 Follower 节点的 MsgHeartbeat 消息之后，会比较对应的 Match 值与 Leader 节点的日志最后一条索引，
		if pr.Match < r.raftLog.lastIndex() {
			r.sendAppend(m.From) // 向指定节点发送 MsgApp 消息，完成 Entry 复制
		}

		if r.readOnly.option != ReadOnlySafe || len(m.Context) == 0 {
			return nil
		}

		// 统计响应 MsgHeartbeatResp 的节点个数， 少于半数不做处理
		if r.prs.Voters.VoteResult(r.readOnly.recvAck(m.From, m.Context)) != quorum.VoteWon {
			return nil
		}
		// 响应节点超过半数后，会清空 readOnly 中指定消息ID 及其之前的所有相关记录
		rss := r.readOnly.advance(m)
		for _, rs := range rss {
			req := rs.req
			// 根据消息的 From 字段，判断该 MsgReadIndex 消息是否为 Follower 节点转发到 Leader 节点的消息
			if req.From == None || req.From == r.id { // from local member
				// 将 MsgReadIndex 消息对应的已提交位置及其消息ID封装成 ReadState 实例，添加到 raft.readStates 中保存
				// 后续会有其他 goroutine 读取该数组，并对响应的 MsgReadIndex 消息进行响应
				r.readStates = append(r.readStates, ReadState{Index: rs.index, RequestCtx: req.Entries[0].Data})
			} else {
				// 如果是其他 Follower 节点转发到 Leader 节点的 MsgReadIndex 消息，则 Leader 节点会向 Follower 节点返回相应的 MsgReadIndexResp 消息，并由Follower 节点响应 Client
				r.send(pb.Message{To: req.From, Type: pb.MsgReadIndexResp, Index: rs.index, Entries: req.Entries})
			}
		}
	case pb.MsgSnapStatus:
		if pr.State != tracker.StateSnapshot {
			return nil
		}
		// TODO(tbg): this code is very similar to the snapshot handling in
		// MsgAppResp above. In fact, the code there is more correct than the
		// code here and should likely be updated to match (or even better, the
		// logic pulled into a newly created Progress state machine handler).
		if !m.Reject {
			// 之前的 MsgSnap 消息发生异常，切换对应 Follower 节点的状态
			pr.BecomeProbe()
			r.logger.Debugf("%x snapshot succeeded, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
		} else {
			// NB: the order here matters or we'll be probing erroneously from
			// the snapshot index, but the snapshot never applied.
			// 发送 MsgSnap 消息失败，这里会清空对应 Process.PendingSnapshot 字段
			pr.PendingSnapshot = 0
			pr.BecomeProbe()
			r.logger.Debugf("%x snapshot failed, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
		}

		// 无论 MsgSnap 消息发送是否失败，都会将对应 Process 切换成 StateProbe 状态，之后发送单条消息进行试探

		// If snapshot finish, wait for the MsgAppResp from the remote node before sending
		// out the next MsgApp.
		// If snapshot failure, wait for a heartbeat interval before next try
		// 暂停 Leader 节点向该 Follower 节点继续发送信息，如果发送 MsgSnap 消息成功了，则待 Leader 节点收到相应信息(MsgAppResp消息)，即可继续发送后续 MsgApp消息
		// 如果发送 MsgSnap 消息失败了，Leader 会等收到 MsgHeartbeatResp 消息时，才会重新开始发送后续消息。
		pr.ProbeSent = true
	case pb.MsgUnreachable:
		// 当 Follower 节点不连通时，继续发送 MsgApp 消息，会有大量消息丢失
		// During optimistic replication, if the remote becomes unreachable,
		// there is huge probability that a MsgApp is lost.
		if pr.State == tracker.StateReplicate {
			pr.BecomeProbe() // 将 Follower 节点对应的 Process 实例切换成 ProcessStateProbe 状态
		}
		r.logger.Debugf("%x failed to send message to %x because it is unreachable [%s]", r.id, m.From, pr)
	case pb.MsgTransferLeader:
		if pr.IsLearner {
			r.logger.Debugf("%x is learner. Ignored transferring leadership", r.id)
			return nil
		}
		leadTransferee := m.From
		lastLeadTransferee := r.leadTransferee
		if lastLeadTransferee != None {
			if lastLeadTransferee == leadTransferee {
				r.logger.Infof("%x [term %d] transfer leadership to %x is in progress, ignores request to same node %x",
					r.id, r.Term, leadTransferee, leadTransferee)
				return nil
			}
			r.abortLeaderTransfer()
			r.logger.Infof("%x [term %d] abort previous transferring leadership to %x", r.id, r.Term, lastLeadTransferee)
		}
		if leadTransferee == r.id {
			r.logger.Debugf("%x is already leader. Ignored transferring leadership to self", r.id)
			return nil
		}
		// Transfer leadership to third party.
		r.logger.Infof("%x [term %d] starts to transfer leadership to %x", r.id, r.Term, leadTransferee)
		// Transfer leadership should be finished in one electionTimeout, so reset r.electionElapsed.
		r.electionElapsed = 0
		r.leadTransferee = leadTransferee
		if pr.Match == r.raftLog.lastIndex() {
			r.sendTimeoutNow(leadTransferee)
			r.logger.Infof("%x sends MsgTimeoutNow to %x immediately as %x already has up-to-date log", r.id, leadTransferee, leadTransferee)
		} else {
			r.sendAppend(leadTransferee)
		}
	}
	return nil
}

// stepCandidate is shared by StateCandidate and StatePreCandidate; the difference is
// whether they respond to MsgVoteResp or MsgPreVoteResp.
func stepCandidate(r *raft, m pb.Message) error {
	// Only handle vote responses corresponding to our candidacy (while in
	// StateCandidate, we may get stale MsgPreVoteResp messages in this term from
	// our pre-candidate state).
	var myVoteRespType pb.MessageType
	if r.state == StatePreCandidate { // 根据当前节点的状态决定其能够处理的选举响应消息的类型
		myVoteRespType = pb.MsgPreVoteResp
	} else {
		myVoteRespType = pb.MsgVoteResp
	}
	switch m.Type {
	case pb.MsgProp:
		r.logger.Infof("%x no leader at term %d; dropping proposal", r.id, r.Term)
		return ErrProposalDropped
	case pb.MsgApp:
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleAppendEntries(m)
	case pb.MsgHeartbeat:
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleHeartbeat(m)
	case pb.MsgSnap:
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleSnapshot(m)
	case myVoteRespType:
		gr, rj, res := r.poll(m.From, m.Type, !m.Reject) // 统计投票结果
		r.logger.Infof("%x has received %d %s votes and %d vote rejections", r.id, gr, m.Type, rj)
		switch res {
		case quorum.VoteWon: // 竞选成功
			if r.state == StatePreCandidate {
				r.campaign(campaignElection) // preVote 通过，发起正式选举请求
			} else {
				r.becomeLeader() // 选举通过，成为新的 Leader
				r.bcastAppend()  // TODO 向其他节点广播 MsgApp 消息， 昭告天下自己成为新的 leader
			}
		case quorum.VoteLost: // 竞选失败
			// pb.MsgPreVoteResp contains future term of pre-candidate
			// m.Term > r.Term; reuse r.Term
			r.becomeFollower(r.Term, None)
		}
	case pb.MsgTimeoutNow:
		r.logger.Debugf("%x [term %d state %v] ignored MsgTimeoutNow from %x", r.id, r.Term, r.state, m.From)
	}
	return nil
}

func stepFollower(r *raft, m pb.Message) error {
	switch m.Type {
	case pb.MsgProp:
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping proposal", r.id, r.Term)
			return ErrProposalDropped
		} else if r.disableProposalForwarding {
			r.logger.Infof("%x not forwarding to leader %x at term %d; dropping proposal", r.id, r.lead, r.Term)
			return ErrProposalDropped
		}
		m.To = r.lead
		r.send(m)
	case pb.MsgApp:
		r.electionElapsed = 0    // 重置选举计算器，防止当前 Follower 发起新一轮的选举
		r.lead = m.From          // 保存当前集群中的 Leader 节点 ID
		r.handleAppendEntries(m) // 将 MsgApp 消息中携带的 Entry 记录追加到 raftLog 中，并且向 Leader 节点 响应 MsgAppResp 消息
	case pb.MsgHeartbeat:
		r.electionElapsed = 0 // 重置选举计时器，防止当前 Follower 发起新一轮的选举
		r.lead = m.From       // 设置当前集群中的 Leader 节点
		// 处理心跳请求消息
		r.handleHeartbeat(m)
	case pb.MsgSnap:
		r.electionElapsed = 0 // 重置 raft.electionElapsed， 防止发生选举
		r.lead = m.From
		r.handleSnapshot(m) // 通过 MsgSnap 中的快照数据，重建当前节点的 raftlog
	case pb.MsgTransferLeader:
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping leader transfer msg", r.id, r.Term)
			return nil
		}
		m.To = r.lead
		r.send(m)
	case pb.MsgTimeoutNow:
		if r.promotable() {
			r.logger.Infof("%x [term %d] received MsgTimeoutNow from %x and starts an election to get leadership.", r.id, r.Term, m.From)
			// Leadership transfers never use pre-vote even if r.preVote is true; we
			// know we are not recovering from a partition so there is no need for the
			// extra round trip.
			r.campaign(campaignTransfer)
		} else {
			r.logger.Infof("%x received MsgTimeoutNow from %x but is not promotable", r.id, m.From)
		}
	case pb.MsgReadIndex:
		// Follower 节点不能直接响应客户端的只读请求，需要转发给 leader 节点处理， 等待 Leader 节点响应后， 再由 Follower 节点响应 Client
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping index reading msg", r.id, r.Term)
			return nil
		}
		m.To = r.lead
		r.send(m)
	case pb.MsgReadIndexResp:
		if len(m.Entries) != 1 {
			r.logger.Errorf("%x invalid format of MsgReadIndexResp from %x, entries count: %d", r.id, m.From, len(m.Entries))
			return nil
		}
		// 将 MsgReadIndex 消息对应的消息 ID 以及已提交位置封装成 ReadState 实例并添加到 raft.readStates 中，等待其他 goroutine 处理
		r.readStates = append(r.readStates, ReadState{Index: m.Index, RequestCtx: m.Entries[0].Data})
	}
	return nil
}

func (r *raft) handleAppendEntries(m pb.Message) {
	// m.Index 表示 leader 发送给 follower 的上一条日志的索引位置
	// 如果 Follower 节点在 m.Index 位置的 Entry 记录已经提交过了，则不能进行追加操作，已提交的记录不能被覆盖，
	// 所以 Follower 节点会将其 committed 位置通过 MsgAppResp 消息的 Index 字段通知 Leader 节点， 即告知 leader 节点正确的 NextIndex 值
	if m.Index < r.raftLog.committed {
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.committed})
		return
	}

	// 尝试将消息携带的 Entry 记录追加到 raftLog 中
	if mlastIndex, ok := r.raftLog.maybeAppend(m.Index, m.LogTerm, m.Commit, m.Entries...); ok {
		// 如果追加成功，则将最后一条记录的索引值通过 MsgAppResp 消息返回给 Leader 节点，这样 Leader 节点就可以根据此值更新其对应的 Next 和 Match 值
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: mlastIndex})
	} else {
		// 如果追加记录失败，则将失败信息返回给 Leader 节点，将 reject 字段设置为 true， 并把当前节点的 raftLog 中的最后一条记录的索引存入 RejectHint
		r.logger.Debugf("%x [logterm: %d, index: %d] rejected MsgApp [logterm: %d, index: %d] from %x",
			r.id, r.raftLog.zeroTermOnErrCompacted(r.raftLog.term(m.Index)), m.Index, m.LogTerm, m.Index, m.From)
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: m.Index, Reject: true, RejectHint: r.raftLog.lastIndex()})
	}
}

func (r *raft) handleHeartbeat(m pb.Message) {
	// 修改本地日志的已提交位置
	r.raftLog.commitTo(m.Commit)
	// 向 Leader 节点发送 MsgHeartbeatResp 响应消息
	r.send(pb.Message{To: m.From, Type: pb.MsgHeartbeatResp, Context: m.Context})
}

// 根据 MsgSnap 消息重建本地 raftLog
func (r *raft) handleSnapshot(m pb.Message) {
	sindex, sterm := m.Snapshot.Metadata.Index, m.Snapshot.Metadata.Term
	// 进行本地日志重建， 返回的 MsgAppResp 消息的 reject 字段始终为 false
	if r.restore(m.Snapshot) {
		// 重建成功
		r.logger.Infof("%x [commit: %d] restored snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, sindex, sterm)
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.lastIndex()})
	} else {
		// 重建失败
		r.logger.Infof("%x [commit: %d] ignored snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, sindex, sterm)
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.committed})
	}
}

// restore recovers the state machine from a snapshot. It restores the log and the
// configuration of state machine. If this method returns false, the snapshot was
// ignored, either because it was obsolete or because of an error.
func (r *raft) restore(s pb.Snapshot) bool {
	if s.Metadata.Index <= r.raftLog.committed {
		// 无需重建
		return false
	}

	if r.state != StateFollower {
		// This is defense-in-depth: if the leader somehow ended up applying a
		// snapshot, it could move into a new term without moving into a
		// follower state. This should never fire, but if it did, we'd have
		// prevented damage by returning early, so log only a loud warning.
		//
		// At the time of writing, the instance is guaranteed to be in follower
		// state when this method is called.
		r.logger.Warningf("%x attempted to restore snapshot as leader; should never happen", r.id)
		r.becomeFollower(r.Term+1, None)
		return false
	}

	// More defense-in-depth: throw away snapshot if recipient is not in the
	// config. This shouldn't ever happen (at the time of writing) but lots of
	// code here and there assumes that r.id is in the progress tracker.
	found := false
	cs := s.Metadata.ConfState
	for _, set := range [][]uint64{
		cs.Voters,
		cs.Learners,
	} {
		for _, id := range set {
			if id == r.id {
				found = true
				break
			}
		}
	}
	if !found {
		r.logger.Warningf(
			"%x attempted to restore snapshot but it is not in the ConfState %v; should never happen",
			r.id, cs,
		)
		return false
	}

	// Now go ahead and actually restore.

	if r.raftLog.matchTerm(s.Metadata.Index, s.Metadata.Term) {
		r.logger.Infof("%x [commit: %d, lastindex: %d, lastterm: %d] fast-forwarded commit to snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, r.raftLog.lastIndex(), r.raftLog.lastTerm(), s.Metadata.Index, s.Metadata.Term)
		r.raftLog.commitTo(s.Metadata.Index)
		return false
	}

	r.raftLog.restore(s)

	// Reset the configuration and add the (potentially updated) peers in anew.
	r.prs = tracker.MakeProgressTracker(r.prs.MaxInflight)
	cfg, prs, err := confchange.Restore(confchange.Changer{
		Tracker:   r.prs,
		LastIndex: r.raftLog.lastIndex(),
	}, cs)

	if err != nil {
		// This should never happen. Either there's a bug in our config change
		// handling or the client corrupted the conf change.
		panic(fmt.Sprintf("unable to restore config %+v: %s", cs, err))
	}

	assertConfStatesEquivalent(r.logger, cs, r.switchToConfig(cfg, prs))

	pr := r.prs.Progress[r.id]
	pr.MaybeUpdate(pr.Next - 1) // TODO(tbg): this is untested and likely unneeded

	r.logger.Infof("%x [commit: %d, lastindex: %d, lastterm: %d] restored snapshot [index: %d, term: %d]",
		r.id, r.raftLog.committed, r.raftLog.lastIndex(), r.raftLog.lastTerm(), s.Metadata.Index, s.Metadata.Term)
	return true
}

// promotable indicates whether state machine can be promoted to leader,
// which is true when its own id is in progress list.
func (r *raft) promotable() bool {
	pr := r.prs.Progress[r.id]
	return pr != nil && !pr.IsLearner
}

func (r *raft) applyConfChange(cc pb.ConfChangeV2) pb.ConfState {
	cfg, prs, err := func() (tracker.Config, tracker.ProgressMap, error) {
		changer := confchange.Changer{
			Tracker:   r.prs,
			LastIndex: r.raftLog.lastIndex(),
		}
		if cc.LeaveJoint() {
			return changer.LeaveJoint()
		} else if autoLeave, ok := cc.EnterJoint(); ok {
			return changer.EnterJoint(autoLeave, cc.Changes...)
		}
		return changer.Simple(cc.Changes...)
	}()

	if err != nil {
		// TODO(tbg): return the error to the caller.
		panic(err)
	}

	return r.switchToConfig(cfg, prs)
}

// switchToConfig reconfigures this node to use the provided configuration. It
// updates the in-memory state and, when necessary, carries out additional
// actions such as reacting to the removal of nodes or changed quorum
// requirements.
//
// The inputs usually result from restoring a ConfState or applying a ConfChange.
func (r *raft) switchToConfig(cfg tracker.Config, prs tracker.ProgressMap) pb.ConfState {
	r.prs.Config = cfg
	r.prs.Progress = prs

	r.logger.Infof("%x switched to configuration %s", r.id, r.prs.Config)
	cs := r.prs.ConfState()
	pr, ok := r.prs.Progress[r.id]

	// Update whether the node itself is a learner, resetting to false when the
	// node is removed.
	r.isLearner = ok && pr.IsLearner

	if (!ok || r.isLearner) && r.state == StateLeader {
		// This node is leader and was removed or demoted. We prevent demotions
		// at the time writing but hypothetically we handle them the same way as
		// removing the leader: stepping down into the next Term.
		//
		// TODO(tbg): step down (for sanity) and ask follower with largest Match
		// to TimeoutNow (to avoid interruption). This might still drop some
		// proposals but it's better than nothing.
		//
		// TODO(tbg): test this branch. It is untested at the time of writing.
		return cs
	}

	// The remaining steps only make sense if this node is the leader and there
	// are other nodes.
	if r.state != StateLeader || len(cs.Voters) == 0 {
		return cs
	}

	if r.maybeCommit() {
		// If the configuration change means that more entries are committed now,
		// broadcast/append to everyone in the updated config.
		r.bcastAppend()
	} else {
		// Otherwise, still probe the newly added replicas; there's no reason to
		// let them wait out a heartbeat interval (or the next incoming
		// proposal).
		r.prs.Visit(func(id uint64, pr *tracker.Progress) {
			r.maybeSendAppend(id, false /* sendIfEmpty */)
		})
	}
	// If the the leadTransferee was removed, abort the leadership transfer.
	if _, tOK := r.prs.Progress[r.leadTransferee]; !tOK && r.leadTransferee != 0 {
		r.abortLeaderTransfer()
	}

	return cs
}

func (r *raft) loadState(state pb.HardState) {
	if state.Commit < r.raftLog.committed || state.Commit > r.raftLog.lastIndex() {
		r.logger.Panicf("%x state.commit %d is out of range [%d, %d]", r.id, state.Commit, r.raftLog.committed, r.raftLog.lastIndex())
	}
	r.raftLog.committed = state.Commit
	r.Term = state.Term
	r.Vote = state.Vote
}

// fix: iff -> if

// pastElectionTimeout returns true iff r.electionElapsed is greater
// than or equal to the randomized election timeout in
// [electiontimeout, 2 * electiontimeout - 1].
func (r *raft) pastElectionTimeout() bool {
	return r.electionElapsed >= r.randomizedElectionTimeout
}

func (r *raft) resetRandomizedElectionTimeout() {
	r.randomizedElectionTimeout = r.electionTimeout + globalRand.Intn(r.electionTimeout)
}

func (r *raft) sendTimeoutNow(to uint64) {
	r.send(pb.Message{To: to, Type: pb.MsgTimeoutNow})
}

func (r *raft) abortLeaderTransfer() {
	r.leadTransferee = None
}

// increaseUncommittedSize computes the size of the proposed entries and
// determines whether they would push leader over its maxUncommittedSize limit.
// If the new entries would exceed the limit, the method returns false. If not,
// the increase in uncommitted entry size is recorded and the method returns
// true.
func (r *raft) increaseUncommittedSize(ents []pb.Entry) bool {
	var s uint64
	for _, e := range ents {
		s += uint64(PayloadSize(e))
	}

	if r.uncommittedSize > 0 && r.uncommittedSize+s > r.maxUncommittedSize {
		// If the uncommitted tail of the Raft log is empty, allow any size
		// proposal. Otherwise, limit the size of the uncommitted tail of the
		// log and drop any proposal that would push the size over the limit.
		return false
	}
	r.uncommittedSize += s
	return true
}

// reduceUncommittedSize accounts for the newly committed entries by decreasing
// the uncommitted entry size limit.
func (r *raft) reduceUncommittedSize(ents []pb.Entry) {
	if r.uncommittedSize == 0 {
		// Fast-path for followers, who do not track or enforce the limit.
		return
	}

	var s uint64
	for _, e := range ents {
		s += uint64(PayloadSize(e))
	}
	if s > r.uncommittedSize {
		// uncommittedSize may underestimate the size of the uncommitted Raft
		// log tail but will never overestimate it. Saturate at 0 instead of
		// allowing overflow.
		r.uncommittedSize = 0
	} else {
		r.uncommittedSize -= s
	}
}

func numOfPendingConf(ents []pb.Entry) int {
	n := 0
	for i := range ents {
		if ents[i].Type == pb.EntryConfChange {
			n++
		}
	}
	return n
}
