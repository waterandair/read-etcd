// Copyright 2019 The etcd Authors
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

package tracker

// StateType is the state of a tracked follower.
type StateType uint64

const (
	// StateProbe indicates a follower whose last index isn't known. Such a
	// follower is "probed" (i.e. an append sent periodically) to narrow down
	// its last index. In the ideal (and common) case, only one round of probing
	// is necessary as the follower will react with a hint. Followers that are
	// probed over extended periods of time are often offline.
	// 表示 Leader 节点一次不能向目标节点发送多条消息，只能待一条消息被响应后，才能发送下一条消息。当刚刚复制完快照数据，上次 MsgApp 消息被
	// 拒绝（或是发送失败）或者 Leader 节点初始化时，都会导致目标节点的 Process 切换到该状态
	StateProbe StateType = iota
	// StateReplicate is the state steady in which a follower eagerly receives
	// log entries to append to its log.
	// 表示正常的 Entry 记录复制状态， Leader 节点向目标节点发送完消息后，无需等待响应，即可开始后续消息的发送
	StateReplicate
	// StateSnapshot indicates a follower that needs log entries not available
	// from the leader's Raft log. Such a follower needs a full snapshot to
	// return to StateReplicate.
	StateSnapshot  // 表示 Leader 节点正在向目标节点发送快照数据

	// 状态转换:
	// StateReplicate -> StateSnapshot : 查找待发送的 Entry 记录失败
	// StateSnapshot -> StateProbe: 快照消息发送完成（无论是否被拒绝）
	// StateProbe -> StateSnapshot: 查找记录失败
	// StateReplicate -> StateProbe: MsgApp 消息被拒绝或是发送
	// StateProbe -> StateReplicate: 消息发送成功之后
)

var prstmap = [...]string{
	"StateProbe",
	"StateReplicate",
	"StateSnapshot",
}

func (st StateType) String() string { return prstmap[uint64(st)] }
