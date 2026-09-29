package document

import "sync"

const defaultPersistentIndexCacheDiskQuotaBytes int64 = 64 * 1024 * 1024

var documentSettings = struct {
	mu                sync.RWMutex
	cacheMaxBytes     int
	persistIndexCache bool
	indexCacheQuota   int64
}{
	cacheMaxBytes:     defaultChunkCacheMaxBytes,
	persistIndexCache: true,
	indexCacheQuota:   defaultPersistentIndexCacheDiskQuotaBytes,
}

func SetCacheMaxBytes(maxBytes int) {
	documentSettings.mu.Lock()
	if maxBytes <= 0 {
		maxBytes = defaultChunkCacheMaxBytes
	}
	documentSettings.cacheMaxBytes = maxBytes
	documentSettings.mu.Unlock()
}

func currentCacheMaxBytes() int {
	documentSettings.mu.RLock()
	defer documentSettings.mu.RUnlock()
	return documentSettings.cacheMaxBytes
}

func SetPersistentIndexCache(enabled bool) {
	documentSettings.mu.Lock()
	documentSettings.persistIndexCache = enabled
	documentSettings.mu.Unlock()
}

func persistentIndexCacheEnabled() bool {
	documentSettings.mu.RLock()
	defer documentSettings.mu.RUnlock()
	return documentSettings.persistIndexCache
}

// SetPersistentIndexCacheDiskQuotaBytes sets the maximum aggregate size of
// recognized persistent line-index cache files. Non-positive values restore
// the conservative default. The quota excludes unrelated and non-regular
// directory entries, which Quarry never removes during eviction.
func SetPersistentIndexCacheDiskQuotaBytes(maxBytes int64) {
	documentSettings.mu.Lock()
	if maxBytes <= 0 {
		maxBytes = defaultPersistentIndexCacheDiskQuotaBytes
	}
	documentSettings.indexCacheQuota = maxBytes
	documentSettings.mu.Unlock()
}

func currentPersistentIndexCacheDiskQuotaBytes() int64 {
	documentSettings.mu.RLock()
	defer documentSettings.mu.RUnlock()
	return documentSettings.indexCacheQuota
}

func (d *FileDocument) SetCacheMaxBytes(maxBytes int) {
	if d == nil || d.cache == nil {
		return
	}
	d.cache.setMaxBytes(maxBytes)
}
