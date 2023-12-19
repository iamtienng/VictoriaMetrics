package streamaggr

import (
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promutils"
)

type pushSamplesFunc func(bb *bytesutil.ByteBuffer, labels *promutils.Labels, tmpLabels *promutils.Labels, key string, value float64)

type deduplicator struct {
	interval       time.Duration
	wg             sync.WaitGroup
	stopCh         chan struct{}
	m              sync.Map
	pushSamplesAgg pushSamplesFunc
}

func newDeduplicator(
	dedupInterval time.Duration,
) *deduplicator {
	return &deduplicator{
		interval: dedupInterval,
		stopCh:   make(chan struct{}),
	}
}

func (d *deduplicator) run(pushSamplesAgg pushSamplesFunc) {
	d.pushSamplesAgg = pushSamplesAgg
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		t := time.NewTicker(d.interval)
		t.Stop()
		defer t.Stop()
		for {
			select {
			case <-d.stopCh:
				return
			case <-t.C:
			}
			d.flush()
		}
	}()
}

func (d *deduplicator) stop() {
	if d == nil {
		return
	}
	close(d.stopCh)
	d.wg.Wait()
	d.flush()
}

func (d *deduplicator) pushSample(key string, value float64) {
again:
	v, ok := d.m.Load(key)
	if !ok {
		// The entry is missing in the map. Try creating it.
		v = &dedupStateValue{value: value}
		vNew, loaded := d.m.LoadOrStore(key, v)
		if !loaded {
			// The new entry has been successfully created.
			return
		}
		// Use the entry created by a concurrent goroutine.
		v = vNew
	}
	sv := v.(*dedupStateValue)
	sv.mu.Lock()
	deleted := sv.deleted
	if !deleted {
		sv.value = value
	}
	sv.mu.Unlock()
	if deleted {
		// The entry has been deleted by the concurrent call to appendSeriesForFlush
		// Try obtaining and updating the entry again.
		goto again
	}
}

func (d *deduplicator) flush() {
	if d == nil {
		return
	}

	labels := promutils.GetLabels()
	tmpLabels := promutils.GetLabels()
	bb := bbPool.Get()

	d.m.Range(func(k, v interface{}) bool {
		// Atomically delete the entry from the map, so new entry is created for the next flush.
		d.m.Delete(k)

		sv := v.(*dedupStateValue)
		sv.mu.Lock()
		value := sv.value
		// Mark the entry as deleted, so it won't be updated anymore by concurrent pushSample() calls.
		sv.deleted = true
		sv.mu.Unlock()

		d.pushSamplesAgg(bb, labels, tmpLabels, k.(string), value)
		return true
	})

	bbPool.Put(bb)
	promutils.PutLabels(tmpLabels)
	promutils.PutLabels(labels)
}

type dedupStateValue struct {
	mu      sync.Mutex
	value   float64
	deleted bool
}