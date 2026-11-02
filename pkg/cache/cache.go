package cache

import (
	"fmt"
	"sync"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"

	log "github.com/sirupsen/logrus"
)

type CacheAllocator struct {
	// mutex guards ipPoolCache: all three controller workers share a single
	// CacheAllocator instance, so every map access must be synchronized
	// (a concurrent map read/write is an unrecoverable runtime fatal)
	mutex       sync.RWMutex
	ipPoolCache map[string]kihv1.IPPool
}

func NewCacheAllocator() *CacheAllocator {
	ipPoolCache := make(map[string]kihv1.IPPool)

	return &CacheAllocator{
		ipPoolCache: ipPoolCache,
	}
}

func New() *CacheAllocator {
	return NewCacheAllocator()
}

func (c *CacheAllocator) Add(t interface{}) (err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	switch t.(type) {
	case *kihv1.IPPool:
		log.Debugf("(cache.Add) adding pool for %s", t.(*kihv1.IPPool).Spec.NetworkName)

		if _, exists := c.ipPoolCache[t.(*kihv1.IPPool).Spec.NetworkName]; exists {
			return fmt.Errorf("IPPool %s already exists in cache", t.(*kihv1.IPPool).Spec.NetworkName)
		}

		// deep-copy the object into the cache: a shallow struct copy would
		// share the nested slices and maps with the source object, so
		// mutating the source would silently rewrite the cached pool too
		copiedPool := t.(*kihv1.IPPool).DeepCopy()
		c.ipPoolCache[t.(*kihv1.IPPool).Spec.NetworkName] = *copiedPool
	}

	return
}

func (c *CacheAllocator) Check(t interface{}) bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	switch t.(type) {
	case *kihv1.IPPool:
		_, exists := c.ipPoolCache[t.(*kihv1.IPPool).Spec.NetworkName]
		return exists
	}

	return false
}

func (c *CacheAllocator) Get(t string, name string) (i interface{}, err error) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	switch t {
	case "pool":
		log.Debugf("(cache.Get) returning pool for %s", name)

		if _, exists := c.ipPoolCache[name]; !exists {
			return i, fmt.Errorf("IPPool %s does not exists in cache", name)
		}

		// return a deep copy as a value, so callers cannot mutate the cached
		// pool through its nested slices and maps while the callers keep
		// interpreting providers as kihv1.IPPool values
		stored := c.ipPoolCache[name]

		return *stored.DeepCopy(), nil
	}

	return
}

// List returns a deep copy of every cached pool of the given type: the
// shutdown paths of the application enumerate the locally registered pools
// without the api, and the copies keep the callers from mutating the cached
// objects while they keep interpreting them as kihv1.IPPool values (the
// same contract as Get).
func (c *CacheAllocator) List(t string) (pools []kihv1.IPPool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	switch t {
	case "pool":
		log.Debugf("(cache.List) returning %d pools", len(c.ipPoolCache))

		for _, stored := range c.ipPoolCache {
			pools = append(pools, *stored.DeepCopy())
		}
	}

	return
}

func (c *CacheAllocator) Delete(t string, name string) (err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	switch t {
	case "pool":
		log.Debugf("(cache.Delete) deleting pool for %s", name)

		if _, exists := c.ipPoolCache[name]; !exists {
			return fmt.Errorf("IPPool %s does not exists in cache", name)
		}

		delete(c.ipPoolCache, name)
	}

	return
}

// Upsert replaces the cached pool under a single lock acquisition: a
// reload which deletes and re-adds the entry in separate steps exposes a
// transient "pool missing" window to the concurrent readers of the shared
// cache, and a reader which acts on it (a cleanup which aborts, a binding
// which fails its restore) diverges from the live state. the entry is
// created when it does not exist yet, so the caller does not have to know
// whether the pool is already cached. the value is deep-copied with the
// same contract as Add.
func (c *CacheAllocator) Upsert(t interface{}) (err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	switch t.(type) {
	case *kihv1.IPPool:
		log.Debugf("(cache.Upsert) replacing pool for %s", t.(*kihv1.IPPool).Spec.NetworkName)

		copiedPool := t.(*kihv1.IPPool).DeepCopy()
		c.ipPoolCache[t.(*kihv1.IPPool).Spec.NetworkName] = *copiedPool
	}

	return
}

func (c *CacheAllocator) Usage(t string) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	switch t {
	case "pool":
		for subnet, pool := range c.ipPoolCache {
			log.Infof("(cache.Usage) ipPoolCache: key=%s, subnet=%s, network=%s, serverip=%s",
				subnet, pool.Spec.IPv4Config.Subnet, pool.Spec.NetworkName, pool.Spec.IPv4Config.ServerIP)
		}
	}
}
