//! Event hub: a bounded ring of sequenced events plus a broadcast channel for live
//! subscribers. Late subscribers may replay from `since`; if the ring has moved past
//! their cursor they get `events_lost` and must resync with a full list.

use canopy_proto::{Event, EventKind};
use std::collections::VecDeque;
use std::sync::Mutex;
use tokio::sync::broadcast;

pub const RING_CAPACITY: usize = 512;

pub struct EventHub {
    ring: Mutex<VecDeque<Event>>,
    next_seq: Mutex<u64>,
    tx: broadcast::Sender<Event>,
}

impl Default for EventHub {
    fn default() -> Self {
        Self::new()
    }
}

impl EventHub {
    pub fn new() -> Self {
        let (tx, _) = broadcast::channel(RING_CAPACITY);
        Self { ring: Mutex::new(VecDeque::with_capacity(RING_CAPACITY)), next_seq: Mutex::new(1), tx }
    }

    pub fn publish(&self, kind: EventKind) -> Event {
        let seq = {
            let mut n = self.next_seq.lock().expect("seq lock");
            let s = *n;
            *n += 1;
            s
        };
        let ev = Event { seq, kind };
        {
            let mut ring = self.ring.lock().expect("ring lock");
            if ring.len() == RING_CAPACITY {
                ring.pop_front();
            }
            ring.push_back(ev.clone());
        }
        let _ = self.tx.send(ev.clone());
        ev
    }

    pub fn subscribe(&self) -> broadcast::Receiver<Event> {
        self.tx.subscribe()
    }

    /// Events with `seq > since`. `Err(oldest)` when `since` predates the ring.
    pub fn replay(&self, since: u64) -> Result<Vec<Event>, u64> {
        let ring = self.ring.lock().expect("ring lock");
        if let Some(oldest) = ring.front() {
            if since + 1 < oldest.seq {
                return Err(oldest.seq);
            }
        }
        Ok(ring.iter().filter(|e| e.seq > since).cloned().collect())
    }

    pub fn latest_seq(&self) -> u64 {
        *self.next_seq.lock().expect("seq lock") - 1
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sequences_and_replays() {
        let hub = EventHub::new();
        let mut rx = hub.subscribe();
        hub.publish(EventKind::WorkspaceCreated { workspace_id: "w1".into() });
        hub.publish(EventKind::WorkspaceUpdated { workspace_id: "w1".into() });
        assert_eq!(hub.latest_seq(), 2);
        assert_eq!(hub.replay(0).unwrap().len(), 2);
        assert_eq!(hub.replay(1).unwrap().len(), 1);
        assert_eq!(rx.try_recv().unwrap().seq, 1);
    }

    #[test]
    fn ring_bounds_and_loss() {
        let hub = EventHub::new();
        for _ in 0..(RING_CAPACITY + 10) {
            hub.publish(EventKind::ServerShutdown);
        }
        assert_eq!(hub.replay(0), Err(11));
        assert_eq!(hub.replay(RING_CAPACITY as u64 + 5).unwrap().len(), 5);
    }
}
