package raft

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/armon/go-metrics"
)

const (
	maxFailureScale = 14
	failureWait     = 10 * time.Millisecond
)

var (
	// ErrLogNotFound indicates a given log entry is not available.
	ErrLogNotFound = errors.New("log not found")
)

type followerReplication struct {
	peer     net.Addr
	inflight *inflight

	stopCh    chan uint64
	triggerCh chan struct{}

	currentTerm uint64
	matchIndex  uint64
	nextIndex   uint64

	lastContact     time.Time
	lastContactLock sync.RWMutex

	failures uint64

	notifyCh   chan struct{}
	notify     []*verifyFuture
	notifyLock sync.Mutex

	// stepDown is used to indicate to the leader that we
	// should step down based on information from a follower.
	stepDown chan struct{}

	// allowPipeline is used to control it seems like
	// pipeline replication should be enabled
	allowPipeline bool
}

// notifyAll is used to notify all the waiting verify futures
// if the follower believes we are still the leader
func (s *followerReplication) notifyAll(leader bool) {
	// Clear the waiting notifies minimizing lock time
	s.notifyLock.Lock()
	n := s.notify
	s.notify = nil
	s.notifyLock.Unlock()

	// Submit our votes
	for _, v := range n {
		v.vote(leader)
	}
}

// LastContact returns the time of last contact
func (s *followerReplication) LastContact() time.Time {
	s.lastContactLock.RLock()
	last := s.lastContact
	s.lastContactLock.RUnlock()
	return last
}

// setLastContact sets the last contact to the current time
func (s *followerReplication) setLastContact() {
	s.lastContactLock.Lock()
	s.lastContact = time.Now()
	s.lastContactLock.Unlock()
}

// replicate is a long running routine that is used to manage
// the process of replicating logs to our followers
func (r *Raft) replicate(s *followerReplication) {
	// Start an async heartbeating routing
	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)
	r.goFunc(func() { r.heartbeat(s, stopHeartbeat) })

RPC:
	shouldStop := false
	for !shouldStop {
		select {
		case maxIndex := <-s.stopCh:
			// Make a best effort to replicate up to this index
			if maxIndex > 0 {
				r.replicateTo(s, maxIndex)
			}
			return
		case <-s.triggerCh:
			shouldStop = r.replicateTo(s, r.getLastLogIndex())
		case <-randomTimeout(r.conf.CommitTimeout):
			shouldStop = r.replicateTo(s, r.getLastLogIndex())
		}

		// If things looks healthy, switch to pipeline mode
		if !shouldStop && s.allowPipeline {
			goto PIPELINE
		}
	}
	return

PIPELINE:
	// Disable until re-enabled
	s.allowPipeline = false

	// Replicates using a pipeline for high performance. This method
	// is not able to gracefully recover from errors, and so we fall back
	// to standard mode on failure.
	if err := r.pipelineReplicate(s); err != nil {
		r.logger.Printf("[ERR] raft: Failed to start pipeline replication to %s: %s", s.peer, err)
	}
	goto RPC
}

// replicateTo is used to replicate the logs up to a given last index.
// If the follower log is behind, we take care to bring them up to date
func (r *Raft) replicateTo(s *followerReplication, lastIndex uint64) (shouldStop bool) {
	// Create the base request
	var l Log
	var req AppendEntriesRequest
	var resp AppendEntriesResponse
	var maxIndex uint64
	var start time.Time
START:
	// Prevent an excessive retry rate on errors
	if s.failures > 0 {
		select {
		case <-time.After(backoff(failureWait, s.failures, maxFailureScale)):
		case <-r.shutdownCh:
		}
	}

	req = AppendEntriesRequest{
		Term:              s.currentTerm,
		Leader:            r.trans.EncodePeer(r.localAddr),
		LeaderCommitIndex: r.getCommitIndex(),
	}

	// Get the previous log entry based on the nextIndex.
	// Guard for the first index, since there is no 0 log entry
	// Guard against the previous index being a snapshot as well
	if s.nextIndex == 1 {
		req.PrevLogEntry = 0
		req.PrevLogTerm = 0

	} else if (s.nextIndex - 1) == r.getLastSnapshotIndex() {
		req.PrevLogEntry = r.getLastSnapshotIndex()
		req.PrevLogTerm = r.getLastSnapshotTerm()

	} else {
		if err := r.logs.GetLog(s.nextIndex-1, &l); err != nil {
			if err == ErrLogNotFound {
				goto SEND_SNAP
			}
			r.logger.Printf("[ERR] raft: Failed to get log at index %d: %v",
				s.nextIndex-1, err)
			return
		}

		// Set the previous index and term (0 if nextIndex is 1)
		req.PrevLogEntry = l.Index
		req.PrevLogTerm = l.Term
	}

	// Append up to MaxAppendEntries or up to the lastIndex
	req.Entries = make([]*Log, 0, r.conf.MaxAppendEntries)
	maxIndex = min(s.nextIndex+uint64(r.conf.MaxAppendEntries)-1, lastIndex)
	for i := s.nextIndex; i <= maxIndex; i++ {
		oldLog := new(Log)
		if err := r.logs.GetLog(i, oldLog); err != nil {
			if err == ErrLogNotFound {
				goto SEND_SNAP
			}
			r.logger.Printf("[ERR] raft: Failed to get log at index %d: %v", i, err)
			return
		}
		req.Entries = append(req.Entries, oldLog)
	}

	// Make the RPC call
	start = time.Now()
	if err := r.trans.AppendEntries(s.peer, &req, &resp); err != nil {
		r.logger.Printf("[ERR] raft: Failed to AppendEntries to %v: %v", s.peer, err)
		s.failures++
		return
	}
	metrics.MeasureSince([]string{"raft", "replication", "appendEntries", "rpc", s.peer.String()}, start)
	metrics.IncrCounter([]string{"raft", "replication", "appendEntries", "logs", s.peer.String()}, float32(len(req.Entries)))

	// Check for a newer term, stop running
	if resp.Term > req.Term {
		r.logger.Printf("[ERR] raft: peer %v has newer term, stopping replication", s.peer)
		s.notifyAll(false) // No longer leader
		asyncNotifyCh(s.stepDown)
		return true
	}

	// Update the last contact
	s.setLastContact()

	// Update the s based on success
	if resp.Success {
		// Mark any inflight logs as committed
		s.inflight.CommitRange(s.nextIndex, maxIndex)

		// Update the indexes
		s.matchIndex = maxIndex
		s.nextIndex = maxIndex + 1

		// Clear any failures
		s.failures = 0

		// Notify still leader
		s.notifyAll(true)

		// We are now in-sync, enable pipelining
		s.allowPipeline = true
	} else {
		s.nextIndex = max(min(s.nextIndex-1, resp.LastLog+1), 1)
		s.matchIndex = s.nextIndex - 1
		s.failures++
		r.logger.Printf("[WARN] raft: AppendEntries to %v rejected, sending older logs (next: %d)", s.peer, s.nextIndex)
	}

CHECK_MORE:
	// Check if there are more logs to replicate
	if s.nextIndex <= lastIndex {
		goto START
	}
	return

	// SEND_SNAP is used when we fail to get a log, usually because the follower
	// is too far behind, and we must ship a snapshot down instead
SEND_SNAP:
	stop, err := r.sendLatestSnapshot(s)

	// Check if we should stop
	if stop {
		return true
	}

	// Check for an error
	if err != nil {
		r.logger.Printf("[ERR] raft: Failed to send snapshot to %v: %v", s.peer, err)
		return
	}

	// Check if there is more to replicate
	goto CHECK_MORE
}

// sendLatestSnapshot is used to send the latest snapshot we have
// down to our follower
func (r *Raft) sendLatestSnapshot(s *followerReplication) (bool, error) {
	// Get the snapshots
	snapshots, err := r.snapshots.List()
	if err != nil {
		r.logger.Printf("[ERR] raft: Failed to list snapshots: %v", err)
		return false, err
	}

	// Check we have at least a single snapshot
	if len(snapshots) == 0 {
		return false, fmt.Errorf("no snapshots found")
	}

	// Open the most recent snapshot
	snapID := snapshots[0].ID
	meta, snapshot, err := r.snapshots.Open(snapID)
	if err != nil {
		r.logger.Printf("[ERR] raft: Failed to open snapshot %v: %v", snapID, err)
		return false, err
	}
	defer snapshot.Close()

	// Setup the request
	req := InstallSnapshotRequest{
		Term:         s.currentTerm,
		Leader:       r.trans.EncodePeer(r.localAddr),
		LastLogIndex: meta.Index,
		LastLogTerm:  meta.Term,
		Peers:        meta.Peers,
		Size:         meta.Size,
	}

	// Make the call
	start := time.Now()
	var resp InstallSnapshotResponse
	if err := r.trans.InstallSnapshot(s.peer, &req, &resp, snapshot); err != nil {
		r.logger.Printf("[ERR] raft: Failed to install snapshot %v: %v", snapID, err)
		s.failures++
		return false, err
	}
	metrics.MeasureSince([]string{"raft", "replication", "installSnapshot", s.peer.String()}, start)

	// Check for a newer term, stop running
	if resp.Term > req.Term {
		r.logger.Printf("[ERR] raft: peer %v has newer term, stopping replication", s.peer)
		s.notifyAll(false) // No longer leader
		asyncNotifyCh(s.stepDown)
		return true, nil
	}

	// Update the last contact
	s.setLastContact()

	// Check for success
	if resp.Success {
		// Mark any inflight logs as committed
		s.inflight.CommitRange(s.matchIndex+1, meta.Index)

		// Update the indexes
		s.matchIndex = meta.Index
		s.nextIndex = s.matchIndex + 1

		// Clear any failures
		s.failures = 0

		// Notify we are still leader
		s.notifyAll(true)
	} else {
		s.failures++
		r.logger.Printf("[WARN] raft: InstallSnapshot to %v rejected", s.peer)
	}
	return false, nil
}

// hearbeat is used to periodically invoke AppendEntries on a peer
// to ensure they don't time out. This is done async of replicate(),
// since that routine could potentially be blocked on disk IO
func (r *Raft) heartbeat(s *followerReplication, stopCh chan struct{}) {
	var failures uint64
	req := AppendEntriesRequest{
		Term:   s.currentTerm,
		Leader: r.trans.EncodePeer(r.localAddr),
	}
	var resp AppendEntriesResponse
	for {
		// Wait for the next heartbeat interval or forced notify
		select {
		case <-s.notifyCh:
		case <-randomTimeout(r.conf.HeartbeatTimeout / 10):
		case <-stopCh:
			return
		}

		start := time.Now()
		if err := r.trans.AppendEntries(s.peer, &req, &resp); err != nil {
			r.logger.Printf("[ERR] raft: Failed to heartbeat to %v: %v", s.peer, err)
			failures++
			select {
			case <-time.After(backoff(failureWait, failures, maxFailureScale)):
			case <-stopCh:
			}
		} else {
			s.setLastContact()
			failures = 0
			metrics.MeasureSince([]string{"raft", "replication", "heartbeat", s.peer.String()}, start)
			s.notifyAll(resp.Success)
		}
	}
}

// pipelineReplicate is used when we have syncronized our state with the follower,
// and want to switch to a higher performance pipeline mode of replication.
// We only pipeline AppendEntries commands, and if we ever hit an error, we fall
// back to the standard replication which can handle more complex situations.
func (r *Raft) pipelineReplicate(s *followerReplication) error {
	// Create a new pipeline
	pipeline, err := r.trans.AppendEntriesPipeline(s.peer)
	if err != nil {
		return err
	}
	defer pipeline.Close()

	// Log start and stop of pipeline
	r.logger.Printf("[INFO] raft: pipelining replication to peer %v", s.peer)
	defer r.logger.Printf("[INFO] raft: aborting pipeline replication to peer %v", s.peer)

	// Create a shutdown and finish channel
	stopCh := make(chan struct{})
	finishCh := make(chan struct{})

	// Start a dedicated decoder
	r.goFunc(func() { r.pipelineDecode(s, pipeline, stopCh, finishCh) })

	// Start pipeline sends at the last good nextIndex
	nextIndex := s.nextIndex

	// Send data as available
	shouldStop := false
SEND:
	for !shouldStop {
		select {
		case <-finishCh:
			break SEND
		case maxIndex := <-s.stopCh:
			if maxIndex > 0 {
				r.pipelineSend(s, pipeline, &nextIndex, maxIndex)
			}
			break SEND
		case <-s.triggerCh:
			shouldStop = r.pipelineSend(s, pipeline, &nextIndex, r.getLastLogIndex())
		case <-randomTimeout(r.conf.CommitTimeout):
			shouldStop = r.pipelineSend(s, pipeline, &nextIndex, r.getLastLogIndex())
		}
	}

	// Stop our decoder
	close(stopCh)

	// Wait for our decoder to finish
	select {
	case <-finishCh:
	case <-r.shutdownCh:
	}
	return nil
}

// pipelineSend is used to send data over a pipeline
func (r *Raft) pipelineSend(s *followerReplication, p AppendPipeline, nextIdx *uint64, lastIndex uint64) (shouldStop bool) {
	// Create a new append request
	req := &AppendEntriesRequest{
		Term:              s.currentTerm,
		Leader:            r.trans.EncodePeer(r.localAddr),
		LeaderCommitIndex: r.getCommitIndex(),
	}

	// Get the previous log entry based on the nextIndex.
	// Guard for the first index, since there is no 0 log entry
	// Guard against the previous index being a snapshot as well
	nextIndex := *nextIdx
	if nextIndex == 1 {
		req.PrevLogEntry = 0
		req.PrevLogTerm = 0

	} else if (nextIndex - 1) == r.getLastSnapshotIndex() {
		req.PrevLogEntry = r.getLastSnapshotIndex()
		req.PrevLogTerm = r.getLastSnapshotTerm()

	} else {
		var l Log
		if err := r.logs.GetLog(nextIndex-1, &l); err != nil {
			if err == ErrLogNotFound {
				return true
			}
			r.logger.Printf("[ERR] raft: Failed to get log at index %d: %v",
				nextIndex-1, err)
			return true
		}

		// Set the previous index and term (0 if nextIndex is 1)
		req.PrevLogEntry = l.Index
		req.PrevLogTerm = l.Term
	}

	// Append up to MaxAppendEntries or up to the lastIndex
	req.Entries = make([]*Log, 0, r.conf.MaxAppendEntries)
	maxIndex := min(nextIndex+uint64(r.conf.MaxAppendEntries)-1, lastIndex)
	for i := nextIndex; i <= maxIndex; i++ {
		oldLog := new(Log)
		if err := r.logs.GetLog(i, oldLog); err != nil {
			if err == ErrLogNotFound {
				return true
			}
			r.logger.Printf("[ERR] raft: Failed to get log at index %d: %v", i, err)
			return true
		}
		req.Entries = append(req.Entries, oldLog)
	}

	// Pipeline the append entries
	if _, err := p.AppendEntries(req, new(AppendEntriesResponse)); err != nil {
		r.logger.Printf("[ERR] raft: Failed to pipeline AppendEntries to %v: %v", s.peer, err)
		return true
	}

	// Increase the next send log to prevent overlap
	*nextIdx = maxIndex + 1
	return false
}

// pipelineDecode is used to decode the responses of pipelined requests
func (r *Raft) pipelineDecode(s *followerReplication, p AppendPipeline, stopCh, finishCh chan struct{}) {
	defer close(finishCh)
	respCh := p.Consumer()
	for {
		select {
		case ready := <-respCh:
			req := ready.Request()
			resp := ready.Response()

			// Update our metrics
			metrics.MeasureSince([]string{"raft", "replication", "appendEntries", "rpc", s.peer.String()}, ready.Start())
			metrics.IncrCounter([]string{"raft", "replication", "appendEntries", "logs", s.peer.String()}, float32(len(req.Entries)))

			// Check for a newer term, stop running
			if resp.Term > req.Term {
				r.logger.Printf("[ERR] raft: peer %v has newer term, stopping replication", s.peer)
				s.notifyAll(false) // No longer leader
				asyncNotifyCh(s.stepDown)
				return
			}

			// Update the last contact
			s.setLastContact()

			// Abort pipeline if not successful
			if !resp.Success {
				return
			}

			// Mark any inflight logs as committed
			if logs := req.Entries; len(logs) > 0 {
				first := logs[0]
				last := logs[len(logs)-1]
				s.inflight.CommitRange(first.Index, last.Index)

				// Update the indexes
				s.matchIndex = last.Index
				s.nextIndex = last.Index + 1
			}

			// Notify still leader
			s.notifyAll(true)
		case <-stopCh:
			return
		}
	}
}
