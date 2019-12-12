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

// Inflights limits the number of MsgApp (represented by the largest index
// contained within) sent to followers but not yet acknowledged by them. Callers
// use Full() to check whether more messages can be sent, call Add() whenever
// they are sending a new append, and release "quota" via FreeLE() whenever an
// ack is received.
// 记录已发送但未收到响应的 MsgApp 消息
type Inflights struct {
	// the starting index in the buffer
	start int // 记录 buffer 中第一条 MsgApp 消息的下标
	// number of inflights in the buffer
	count int // 当前 inflights 中记录的 MsgApp 消息个数

	// the size of the buffer
	size int // 当前 inflights 中能记录的 MsgApp 消息个数的上限

	// buffer contains the index of the last entry
	// inside one message.
	buffer []uint64 // 环形数组， 用来记录 MsgApp 消息相关信息的数组，其中记录的是 MsgApp 消息中最后一条 Entry 记录的索引值
}

// NewInflights sets up an Inflights that allows up to 'size' inflight messages.
func NewInflights(size int) *Inflights {
	return &Inflights{
		size: size,
	}
}

// Clone returns an *Inflights that is identical to but shares no memory with
// the receiver.
func (in *Inflights) Clone() *Inflights {
	ins := *in
	ins.buffer = append([]uint64(nil), in.buffer...)
	return &ins
}

// Add notifies the Inflights that a new message with the given index is being
// dispatched. Full() must be called prior to Add() to verify that there is room
// for one more message, and consecutive calls to add Add() must provide a
// monotonic sequence of indexes.
func (in *Inflights) Add(inflight uint64) {
	if in.Full() { // 检测当前 buffer 是否已经被填充满了
		panic("cannot add into a Full inflights")
	}
	next := in.start + in.count // 获取新消息的下标
	size := in.size
	if next >= size { // 环形数组，满了后，回到初始位置
		next -= size
	}

	// 初始化时的 buffer 数组较短，随着使用会不断进行扩容，上限为size
	if next >= len(in.buffer) {
		in.grow()
	}
	in.buffer[next] = inflight  // 在 next 的位置记录消息中最后一条 Entry 记录的索引值
	in.count++  // 递增 count 字段
}

// grow the inflight buffer by doubling up to inflights.size. We grow on demand
// instead of preallocating to inflights.size to handle systems which have
// thousands of Raft groups per process.
// 按需增长， 每次成倍增加。
func (in *Inflights) grow() {
	newSize := len(in.buffer) * 2
	if newSize == 0 {
		newSize = 1
	} else if newSize > in.size {
		newSize = in.size
	}
	newBuffer := make([]uint64, newSize)
	copy(newBuffer, in.buffer)
	in.buffer = newBuffer
}

// FreeLE frees the inflights smaller or equal to the given `to` flight.
// 释放在 inflights 中小于等于 `to` 的消息
func (in *Inflights) FreeLE(to uint64) {
	if in.count == 0 || to < in.buffer[in.start] {
		// out of the left side of the window
		// inflights 中没有消息或者已经被清除
		return
	}

	idx := in.start
	var i int  // i 记录了此次释放的消息的个数
	for i = 0; i < in.count; i++ {
		if to < in.buffer[idx] { // found the first large inflight
			// 从 start 开始遍历 buffer， 查找第一个大于指定索引值的位置
			break
		}

		// increase index and maybe rotate
		size := in.size
		if idx++; idx >= size {

			// 因为是环形队列，所以如果 idx 越界， 则中 0 开始继续遍历
			idx -= size
		}
	}
	// free i inflights and set new start index
	in.count -= i
	in.start = idx  // 从 start ~ idx 的所有消息都被释放
	if in.count == 0 {
		// inflights is empty, reset the start index so that we don't grow the buffer unnecessarily.
		// inflights 为空后，重置 start， 这样就可以避免 buffer 不必要的增长
		in.start = 0
	}
}

// FreeFirstOne releases the first inflight. This is a no-op if nothing is
// inflight.
func (in *Inflights) FreeFirstOne() { in.FreeLE(in.buffer[in.start]) }

// Full returns true if no more messages can be sent at the moment.
func (in *Inflights) Full() bool {
	return in.count == in.size
}

// Count returns the number of inflight messages.
func (in *Inflights) Count() int { return in.count }

// reset frees all inflights.
func (in *Inflights) reset() {
	in.count = 0
	in.start = 0
}
