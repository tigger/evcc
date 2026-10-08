package charger

import (
	"fmt"
	"sync"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
)

// eproBlock is a contiguous holding register range that is read with a single request
type eproBlock struct {
	start, qty  uint16
	data        []byte
	fetched     time.Time
	ttl         time.Duration // overrides the cache default if set
	unsupported bool
}

func (b *eproBlock) contains(address, quantity uint16) bool {
	return address >= b.start && int(address)+int(quantity) <= int(b.start)+int(b.qty)
}

// eproCache serves register reads from blocks that are fetched in bulk and reused for a short time
type eproCache struct {
	mu     sync.Mutex
	reader registerReader
	log    *util.Logger
	ttl    time.Duration
	blocks []*eproBlock

	// onFetch is called with the raw data of every block refill, e.g. for debug logging
	onFetch func(start uint16, data []byte)
}

type registerReader interface {
	ReadHoldingRegisters(address, quantity uint16) ([]byte, error)
}

func newEProCache(reader registerReader, log *util.Logger, ttl time.Duration, blocks ...[2]uint16) *eproCache {
	c := &eproCache{reader: reader, log: log, ttl: ttl}
	for _, b := range blocks {
		c.blocks = append(c.blocks, &eproBlock{start: b[0], qty: b[1]})
	}
	return c
}

// setTTL overrides the cache lifetime of the block starting at the given address
func (c *eproCache) setTTL(start uint16, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, b := range c.blocks {
		if b.start == start {
			b.ttl = ttl
		}
	}
}

// read returns the requested registers from the covering block, fetching the block when it is stale.
// Ranges outside of all blocks, and blocks the device rejects, are read directly.
func (c *eproCache) read(address, quantity uint16) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, b := range c.blocks {
		if b.unsupported || !b.contains(address, quantity) {
			continue
		}

		ttl := c.ttl
		if b.ttl > 0 {
			ttl = b.ttl
		}

		if b.data == nil || time.Since(b.fetched) >= ttl {
			data, err := c.reader.ReadHoldingRegisters(b.start, b.qty)
			if err == nil && len(data) != 2*int(b.qty) {
				err = fmt.Errorf("block read %d+%d: invalid length %d", b.start, b.qty, len(data))
			}
			if err != nil {
				b.data = nil

				// a rejected block (e.g. gaps in the register map) must not break reads
				if modbus.IsException(err) {
					b.unsupported = true
					c.log.WARN.Printf("block read %d+%d rejected, falling back to single reads: %v", b.start, b.qty, err)
					break
				}

				return nil, err
			}

			b.data, b.fetched = data, time.Now()
			if c.onFetch != nil {
				c.onFetch(b.start, data)
			}
		}

		offset := 2 * int(address-b.start)
		return append([]byte(nil), b.data[offset:offset+2*int(quantity)]...), nil
	}

	return c.reader.ReadHoldingRegisters(address, quantity)
}

// write stores successfully written registers in the covering block, so that the next read needs no request.
// All other blocks are dropped as their values may depend on the write.
func (c *eproCache) write(address uint16, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, b := range c.blocks {
		if quantity := uint16(len(data) / 2); b.data != nil && b.contains(address, quantity) {
			copy(b.data[2*int(address-b.start):], data)
			continue
		}
		b.data = nil
	}
}

// invalidate drops all cached blocks, to be called after failed writes
func (c *eproCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, b := range c.blocks {
		b.data = nil
	}
}
