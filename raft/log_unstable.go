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

import pb "go.etcd.io/etcd/raft/raftpb"

// unstable.entries[i] has raft log position i+unstable.offset.
// Note that unstable.offset may be less than the highest log
// position in storage; this means that the next write to storage
// might need to truncate the log before persisting unstable.entries.

// unstable 用于存储不稳定的 entries 记录:
// 对于 Leader 而言,它维护了客户端请求对应的 Entry 记录
// 对于 Follower 节点而言, 它维护了从 Leader 节点复制来的 Entry 记录
// 无论是 Leader 节点还是 Follower 节点, 对于刚刚接收到的 Entry 记录首先会被存储在 unstable 中.
// 之后,会按照 raft 协议将 unstable 中的 entries 交给上层模块做处理(比如将这些 entries 记录发送到其他节点进行保存(写入Storage)).
// 上层模块完成处理后,会调用 Advance() 方法通知底层的 etcd-raft 模块将 unstable 中对应的 Entry 记录删除.
// unstable 中保存的 entries 记录可能会因为节点故障而意外丢失

type unstable struct {
	// the incoming unstable snapshot, if any.
	snapshot *pb.Snapshot  // 未写入 Storage 的快照数据
	// all entries that have not yet been written to storage.
	entries []pb.Entry  // 记录所有未被存入 Storage 的 Entry 记录
	offset  uint64  // entries 中的第一条 Entry 记录的索引值

	logger Logger
}

// maybeFirstIndex returns the index of the first possible entry in entries
// if it has a snapshot.
// 如果有快照,才会返回第一个索引值, 否则返回 0.
func (u *unstable) maybeFirstIndex() (uint64, bool) {
	if u.snapshot != nil {
		return u.snapshot.Metadata.Index + 1, true
	}
	return 0, false
}

// maybeLastIndex returns the last index if it has at least one
// unstable entry or snapshot.
// 依次从 entries 和快照中取出, 直到取到
func (u *unstable) maybeLastIndex() (uint64, bool) {
	if l := len(u.entries); l != 0 {
		return u.offset + uint64(l) - 1, true
	}
	if u.snapshot != nil {
		return u.snapshot.Metadata.Index, true
	}
	return 0, false
}

// maybeTerm returns the term of the entry at index i, if there is any.
// 取出指定索引位置的任期值
func (u *unstable) maybeTerm(i uint64) (uint64, bool) {
	if i < u.offset {
		// 指定的索引值在 unstable 最小索引值之前,可能已经被持久化或者存入了快照
		if u.snapshot != nil && u.snapshot.Metadata.Index == i {
			return u.snapshot.Metadata.Term, true
		}
		return 0, false
	}

	last, ok := u.maybeLastIndex()
	if !ok {
		return 0, false
	}
	if i > last {
		return 0, false
	}

	return u.entries[i-u.offset].Term, true
}

// 当 unstable.entries 中的 Entry 记录已经被写入Storage 后,会调用 unstable.stableTo() 方法清除 entries 中对应的 Entry 记录
// 将索引值为 i 之前的索引从 unstable 中清除
func (u *unstable) stableTo(i, term uint64) {
	gt, ok := u.maybeTerm(i)
	if !ok {
		// 找不到索引位置 i 的任期值,表示对应的 Entry 不在 unstable 中
		return
	}

	// if i < offset, term is matched with the snapshot, only update the unstable entries if term is matched with an unstable entry.
	// 如果 i < offset, 表示索引位置 1 匹配到了快照, 已经是稳定的 entry了; 这里只更新不稳定的 entry
	if gt == term && i >= u.offset {
		u.entries = u.entries[i+1-u.offset:]
		u.offset = i + 1
		u.shrinkEntriesArray()  // 在底层数组长度超过实际占用的两倍时,对底层数组进行缩减
	}
}

// shrinkEntriesArray discards the underlying array used by the entries slice
// if most of it isn't being used. This avoids holding references to a bunch of
// potentially large entries that aren't needed anymore. Simply clearing the
// entries wouldn't be safe because clients might still be using them.
// 如果
func (u *unstable) shrinkEntriesArray() {
	// We replace the array if we're using less than half of the space in
	// it. This number is fairly arbitrary, chosen as an attempt to balance
	// memory usage vs number of allocations. It could probably be improved
	// with some focused tuning.
	const lenMultiple = 2
	if len(u.entries) == 0 {
		u.entries = nil
	} else if len(u.entries)*lenMultiple < cap(u.entries) {
		newEntries := make([]pb.Entry, len(u.entries))
		copy(newEntries, u.entries)
		u.entries = newEntries
	}
}

func (u *unstable) stableSnapTo(i uint64) {
	if u.snapshot != nil && u.snapshot.Metadata.Index == i {
		u.snapshot = nil
	}
}

func (u *unstable) restore(s pb.Snapshot) {
	u.offset = s.Metadata.Index + 1
	u.entries = nil
	u.snapshot = &s
}

// 向 unstable 中添加 entries, 同 Storage 的 Append 函数类似,也要将传入的 Entries 的索引与已存的索引进行比对
func (u *unstable) truncateAndAppend(ents []pb.Entry) {
	after := ents[0].Index
	switch {
	case after == u.offset+uint64(len(u.entries)):  // 完美契合,直接追加
		// after is the next index in the u.entries
		// directly append
		u.entries = append(u.entries, ents...)
	case after <= u.offset:  // 传入的日志的索引比已存在的最小的还小, 直接替换
		u.logger.Infof("replace the unstable entries from index %d", after)
		// The log is being truncated to before our current offset
		// portion, so set the offset and replace the entries
		u.offset = after
		u.entries = ents
	default:  // 部分包含或超出; 部分包含, 会将包含部分覆盖;
		// truncate to after and copy to u.entries
		// then append
		u.logger.Infof("truncate the unstable entries before index %d", after)
		u.entries = append([]pb.Entry{}, u.slice(u.offset, after)...)
		u.entries = append(u.entries, ents...)
	}
}

func (u *unstable) slice(lo uint64, hi uint64) []pb.Entry {
	u.mustCheckOutOfBounds(lo, hi)
	return u.entries[lo-u.offset : hi-u.offset]
}

// u.offset <= lo <= hi <= u.offset+len(u.entries)
func (u *unstable) mustCheckOutOfBounds(lo, hi uint64) {
	if lo > hi {
		u.logger.Panicf("invalid unstable.slice %d > %d", lo, hi)
	}
	upper := u.offset + uint64(len(u.entries))
	if lo < u.offset || hi > upper {
		u.logger.Panicf("unstable.slice[%d,%d) out of bound [%d,%d]", lo, hi, u.offset, upper)
	}
}
