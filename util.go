package localtunnel

import "sync"

// Atomic counter
type counter struct {
	m       sync.Mutex
	counter int
}

// Add value to counter
func (c *counter) Add(value int) {
	c.m.Lock()
	defer c.m.Unlock()
	c.counter += value
}