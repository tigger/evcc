package charger

import (
	"errors"
	"testing"
	"time"

	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	gridx "github.com/grid-x/modbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRegisters struct {
	calls [][2]uint16
	err   func(address, quantity uint16) error
}

func (f *fakeRegisters) ReadHoldingRegisters(address, quantity uint16) ([]byte, error) {
	f.calls = append(f.calls, [2]uint16{address, quantity})
	if f.err != nil {
		if err := f.err(address, quantity); err != nil {
			return nil, err
		}
	}

	b := make([]byte, 2*quantity)
	for i := range quantity {
		b[2*i+1] = byte(address + i) // low byte identifies the register
	}
	return b, nil
}

func TestEProCacheBlockRead(t *testing.T) {
	f := new(fakeRegisters)
	c := newEProCache(f, util.NewLogger("foo"), time.Minute, [2]uint16{100, 9}, [2]uint16{200, 4})

	b, err := c.read(103, 2)
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 103, 0, 104}, b)

	// served from the same block
	_, err = c.read(100, 1)
	require.NoError(t, err)
	_, err = c.read(108, 1)
	require.NoError(t, err)
	assert.Equal(t, [][2]uint16{{100, 9}}, f.calls)

	// other block and uncovered range
	_, err = c.read(201, 1)
	require.NoError(t, err)
	_, err = c.read(500, 2)
	require.NoError(t, err)
	assert.Equal(t, [][2]uint16{{100, 9}, {200, 4}, {500, 2}}, f.calls)
}

func TestEProCacheInvalidateAndTTL(t *testing.T) {
	f := new(fakeRegisters)
	c := newEProCache(f, util.NewLogger("foo"), time.Minute, [2]uint16{100, 4})

	_, _ = c.read(100, 1)
	c.invalidate()
	_, _ = c.read(100, 1)
	assert.Len(t, f.calls, 2, "refetch after invalidate")

	c.blocks[0].fetched = time.Now().Add(-2 * time.Minute)
	_, _ = c.read(100, 1)
	assert.Len(t, f.calls, 3, "refetch after ttl")
}

func TestEProCacheFallback(t *testing.T) {
	f := &fakeRegisters{err: func(address, quantity uint16) error {
		if quantity > 1 {
			return &gridx.Error{FunctionCode: 3, ExceptionCode: gridx.ExceptionCodeIllegalDataAddress}
		}
		return nil
	}}
	require.True(t, modbus.IsException(f.err(0, 2)))

	c := newEProCache(f, util.NewLogger("foo"), time.Minute, [2]uint16{100, 4})

	b, err := c.read(101, 1)
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 101}, b)

	// block is not retried after rejection
	_, err = c.read(102, 1)
	require.NoError(t, err)
	assert.Equal(t, [][2]uint16{{100, 4}, {101, 1}, {102, 1}}, f.calls)
}

func TestEProCacheTransientError(t *testing.T) {
	errTimeout := errors.New("timeout")
	f := &fakeRegisters{err: func(address, quantity uint16) error { return errTimeout }}
	c := newEProCache(f, util.NewLogger("foo"), time.Minute, [2]uint16{100, 4})

	_, err := c.read(100, 1)
	assert.ErrorIs(t, err, errTimeout)

	f.err = nil
	_, err = c.read(100, 1)
	require.NoError(t, err)
	assert.Equal(t, [][2]uint16{{100, 4}, {100, 4}}, f.calls, "block retried after transient error")
}

func TestEProCacheOnFetch(t *testing.T) {
	f := new(fakeRegisters)
	c := newEProCache(f, util.NewLogger("foo"), time.Minute, [2]uint16{100, 4})

	var fetched []uint16
	c.onFetch = func(start uint16, data []byte) { fetched = append(fetched, start) }

	_, _ = c.read(100, 1)
	_, _ = c.read(101, 1)
	c.invalidate()
	_, _ = c.read(100, 1)

	assert.Equal(t, []uint16{100, 100}, fetched, "once per refill")
}

func TestEProCacheWriteThrough(t *testing.T) {
	f := new(fakeRegisters)
	c := newEProCache(f, util.NewLogger("foo"), time.Minute, [2]uint16{100, 4}, [2]uint16{200, 4})

	_, _ = c.read(100, 1)
	_, _ = c.read(200, 1)
	require.Len(t, f.calls, 2)

	c.write(101, []byte{1, 2})

	b, err := c.read(101, 1)
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2}, b, "written value served from cache")
	assert.Len(t, f.calls, 2, "no read after write for the written block")

	_, _ = c.read(200, 1)
	assert.Len(t, f.calls, 3, "other blocks are refetched")
}

func TestEProCacheBlockTTL(t *testing.T) {
	f := new(fakeRegisters)
	c := newEProCache(f, util.NewLogger("foo"), time.Second, [2]uint16{100, 4}, [2]uint16{200, 4})
	c.setTTL(200, time.Hour)

	_, _ = c.read(100, 1)
	_, _ = c.read(200, 1)
	for _, b := range c.blocks {
		b.fetched = time.Now().Add(-time.Minute)
	}
	_, _ = c.read(100, 1)
	_, _ = c.read(200, 1)

	assert.Equal(t, [][2]uint16{{100, 4}, {200, 4}, {100, 4}}, f.calls, "only the block with default ttl is refetched")
}
