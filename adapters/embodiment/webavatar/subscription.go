package webavatar

import "sync"

// Current returns a value copy of the latest avatar update.
func (d *Driver) Current() Update {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

// Subscribe returns the current snapshot and a capacity-one latest-only update
// stream. The idempotent cancel function removes and closes the subscription.
func (d *Driver) Subscribe() (Update, <-chan Update, func()) {
	d.mu.Lock()
	id := d.nextSubID
	d.nextSubID++
	updates := make(chan Update, 1)
	d.subscribers[id] = updates
	current := d.current
	d.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			d.mu.Lock()
			if existing, ok := d.subscribers[id]; ok {
				delete(d.subscribers, id)
				close(existing)
			}
			d.mu.Unlock()
		})
	}
	return current, updates, cancel
}

func (d *Driver) publishLocked(update Update) {
	d.current.Revision++
	update.Revision = d.current.Revision
	d.current = update
	for _, subscriber := range d.subscribers {
		select {
		case subscriber <- update:
			continue
		default:
		}
		select {
		case <-subscriber:
		default:
		}
		subscriber <- update
	}
}
