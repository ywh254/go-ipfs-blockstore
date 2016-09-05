package blockstore

import (
	"sync/atomic"

	"github.com/ipfs/go-ipfs/blocks"
	key "gx/ipfs/Qmce4Y4zg3sYr7xKM5UueS67vhNni6EeWgCRnb7MbLJMew/go-key"

	"gx/ipfs/QmVWBQQAz4Cd2XgW9KgQoqXXrU8KJoCb9WCrhWRFVBKvFe/go-metrics-interface"
	bloom "gx/ipfs/QmWQ2SJisXwcCLsUXLwYCKSfyExXjFRW2WbBH5sqCUnwX5/bbloom"
	context "gx/ipfs/QmZy2y8t9zQH2a1b8q2ZSLKp17ATuJoCNxxyMFG5qFExpt/go-net/context"
)

// bloomCached returns Blockstore that caches Has requests using Bloom filter
// Size is size of bloom filter in bytes
func bloomCached(bs Blockstore, ctx context.Context, bloomSize, hashCount int) (*bloomcache, error) {
	bl, err := bloom.New(float64(bloomSize), float64(hashCount))
	if err != nil {
		return nil, err
	}
	bc := &bloomcache{blockstore: bs, bloom: bl}
	bc.hits = metrics.NewCtx(ctx, "bloom.hits_total",
		"Number of cache hits in bloom cache").Counter()
	bc.total = metrics.NewCtx(ctx, "bloom_total",
		"Total number of requests to bloom cache").Counter()
	bc.Invalidate()
	go bc.Rebuild(ctx)

	return bc, nil
}

type bloomcache struct {
	bloom  *bloom.Bloom
	active int32

	// This chan is only used for testing to wait for bloom to enable
	rebuildChan chan struct{}
	blockstore  Blockstore

	// Statistics
	hits  metrics.Counter
	total metrics.Counter
}

func (b *bloomcache) Invalidate() {
	b.rebuildChan = make(chan struct{})
	atomic.StoreInt32(&b.active, 0)
}

func (b *bloomcache) BloomActive() bool {
	return atomic.LoadInt32(&b.active) != 0
}

func (b *bloomcache) Rebuild(ctx context.Context) {
	evt := log.EventBegin(ctx, "bloomcache.Rebuild")
	defer evt.Done()

	ch, err := b.blockstore.AllKeysChan(ctx)
	if err != nil {
		log.Errorf("AllKeysChan failed in bloomcache rebuild with: %v", err)
		return
	}
	finish := false
	for !finish {
		select {
		case key, ok := <-ch:
			if ok {
				b.bloom.AddTS([]byte(key)) // Use binary key, the more compact the better
			} else {
				finish = true
			}
		case <-ctx.Done():
			log.Warning("Cache rebuild closed by context finishing.")
			return
		}
	}
	close(b.rebuildChan)
	atomic.StoreInt32(&b.active, 1)
}

func (b *bloomcache) DeleteBlock(k key.Key) error {
	if has, ok := b.hasCached(k); ok && !has {
		return ErrNotFound
	}

	return b.blockstore.DeleteBlock(k)
}

// if ok == false has is inconclusive
// if ok == true then has respons to question: is it contained
func (b *bloomcache) hasCached(k key.Key) (has bool, ok bool) {
	b.total.Inc()
	if k == "" {
		// Return cache invalid so call to blockstore
		// in case of invalid key is forwarded deeper
		return false, false
	}
	if b.BloomActive() {
		blr := b.bloom.HasTS([]byte(k))
		if blr == false { // not contained in bloom is only conclusive answer bloom gives
			b.hits.Inc()
			return false, true
		}
	}
	return false, false
}

func (b *bloomcache) Has(k key.Key) (bool, error) {
	if has, ok := b.hasCached(k); ok {
		return has, nil
	}

	return b.blockstore.Has(k)
}

func (b *bloomcache) Get(k key.Key) (blocks.Block, error) {
	if has, ok := b.hasCached(k); ok && !has {
		return nil, ErrNotFound
	}

	return b.blockstore.Get(k)
}

func (b *bloomcache) Put(bl blocks.Block) error {
	if has, ok := b.hasCached(bl.Key()); ok && has {
		return nil
	}

	err := b.blockstore.Put(bl)
	if err == nil {
		b.bloom.AddTS([]byte(bl.Key()))
	}
	return err
}

func (b *bloomcache) PutMany(bs []blocks.Block) error {
	// bloom cache gives only conclusive resulty if key is not contained
	// to reduce number of puts we need conclusive infomration if block is contained
	// this means that PutMany can't be improved with bloom cache so we just
	// just do a passthrough.
	err := b.blockstore.PutMany(bs)
	if err != nil {
		return err
	}
	for _, bl := range bs {
		b.bloom.AddTS([]byte(bl.Key()))
	}
	return nil
}

func (b *bloomcache) AllKeysChan(ctx context.Context) (<-chan key.Key, error) {
	return b.blockstore.AllKeysChan(ctx)
}

func (b *bloomcache) GCLock() Unlocker {
	return b.blockstore.(GCBlockstore).GCLock()
}

func (b *bloomcache) PinLock() Unlocker {
	return b.blockstore.(GCBlockstore).PinLock()
}

func (b *bloomcache) GCRequested() bool {
	return b.blockstore.(GCBlockstore).GCRequested()
}
