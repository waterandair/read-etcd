#### 各类消息的处理
##### MsgHup 本地消息，发起选举 
`—— 这是集群生命周期中的第一个消息，就像婴儿的第一声啼哭一样美妙`
集群启动一段时间后，会有一个 Follower 节点的选举计时器超时（electionTimeout），此时该节点会创建一个 Term 值为 0 （代表本地消息）的 MsgHup 并将其发送给消息处理函数，
根据当前节点是否开启 PreVote 模式进行下一步处理： 如果开启了 PreVote 模式，会向其他节点发送 MsgPreVote 请求； 如果没有开启 PreVote 模式，会直接向其他节点发送 MsgVote 请求.  

##### MsgPreVote 发起选举投票请求消息
Follower 节点会收到此类消息：  
首先检测这个消息是否是 Leader 节点迁移时发出的及其他合法性检测，然后决定当前节点是否参与此次选举，向消息的发送节点投票
##### MsgPreVoteResp 发起预选举投票的响应消息
PreCandidate 节点会收到此类消息：
当 PreCandidate 节点收到半数以上的投票时，会将自己的状态切换为 Candidate 并发起正式的选举
##### MsgVote 发起选举投票请求消息  
Follower 节点会收到此类消息：  
检测当前节点是和否投票及发送过了 MsgVoteResp 消息，重置当前节点的选举超时计时器，并更新 raft.Vote 字段。
##### MsgVoteResp 选举投票响应消息
Candidate 节点会收到此类消息：  
首先检测当前节点是否通过了选举，即是否收到了半数以上的选票，如果是，则将当前节点切换成 Leader 状态，之后像集群中其他节点发送消息。

##### MsgApp 日志复制请求消息
Follower 节点收到此类消息：
1. 重置选举超时计算器，防止发起新一轮选举
2. 记录当前集群的 leader 节点 ID
3. 判断 MsgApp 消息中保存的 Index（表示 MsgApp 携带的日志的前一条日志的索引） 值是否小于当前节点 raftLog 的最后一个提交的日志索引，如果是，则返回 raftLog.committed, 提示leader 节点从正确的索引位置发送日志
4. 尝试将 MsgApp 携带的 entries 追加到 raftLog 中，根据追加结果返回相应 MsgAppResp 消息


Candidate 节点收到此类消息：
1. 将当前节点的状态转换为 Follower
2. 执行与上述 Follower 节点的逻辑

##### MsgAppResp 日志复制的响应消息
todo

##### MsgBeat 心跳消息和 MsgCheckQuorum 探活消息
这两种消息的 Term 值都为 0， 都属于本地消息。  

Leader 节点除了向集群中其他 Follower 发送 MsgApp 消息，还会向这些 Follower 节点发送 MsgHeartbeat 消息。 Leader 会发送一个 MsgBeat 的本地消息，
处理这个消息的过程就是向 Follower 节点广播 MsgHeartbeat ， 主要作用的心跳探活，
当 follower 节点收到此消息，就会重置自己的选举超时器（election_timeout），防止 Follower 节点发起选举.  


##### MsgHeartbeat 和 MsgHeartbeatResp 消息

  

