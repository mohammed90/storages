package otter

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/darkweak/storages/core"
	"github.com/maypok86/otter"
	lz4 "github.com/pierrec/lz4/v4"
	"go.uber.org/zap"
)

// Otter provider type.
type Otter struct {
	cache  *otter.CacheWithVariableTTL[string, []byte]
	stale  time.Duration
	logger *zap.Logger
}

// Factory function create new Otter instance.
func Factory(otterCfg core.CacheProvider, logger *zap.Logger, stale time.Duration) (core.Storer, error) {
	defaultStorageSize := 10_000
	otterConfiguration := otterCfg.Configuration

	if otterConfiguration != nil {
		if oc, ok := otterConfiguration.(map[string]interface{}); ok {
			if v, found := oc["size"]; found && v != nil {
				if val, ok := v.(int); ok && val > 0 {
					defaultStorageSize = val
				}
			}
		}
	}

	cache, err := otter.MustBuilder[string, []byte](defaultStorageSize).
		CollectStats().
		Cost(func(key string, value []byte) uint32 {
			return 1
		}).
		WithVariableTTL().
		Build()
	if err != nil {
		logger.Sugar().Error("Impossible to instantiate the Otter DB.", err)
	}

	return &Otter{cache: &cache, logger: logger, stale: stale}, nil
}

// Name returns the storer name.
func (provider *Otter) Name() string {
	return "OTTER"
}

// Uuid returns an unique identifier.
func (provider *Otter) Uuid() string {
	return fmt.Sprint(provider.stale)
}

// MapKeys method returns a map with the key and value.
func (provider *Otter) MapKeys(prefix string) map[string]string {
	keys := map[string]string{}

	provider.cache.Range(func(key string, val []byte) bool {
		if !strings.HasPrefix(key, prefix) {
			k, _ := strings.CutPrefix(key, prefix)
			keys[k] = string(val)
		}

		return true
	})

	return keys
}

// ListKeys method returns the list of existing keys.
func (provider *Otter) ListKeys() []string {
	keys := []string{}

	provider.cache.Range(func(key string, value []byte) bool {
		if strings.HasPrefix(key, core.MappingKeyPrefix) {
			mapping, err := core.DecodeMapping(value)
			if err == nil {
				for _, v := range mapping.Mapping {
					keys = append(keys, v.RealKey)
				}
			}
		}

		return true
	})

	return keys
}

// Get method returns the populated response if exists, empty response then.
func (provider *Otter) Get(key string) []byte {
	result, found := provider.cache.Get(key)
	if !found {
		provider.logger.Sugar().Errorf("Impossible to get the key %s in Otter", key)
	}

	return result
}

// GetMultiLevel tries to load the key and check if one of linked keys is a fresh/stale candidate.
func (provider *Otter) GetMultiLevel(key string, req *http.Request, validator *core.Revalidator) (fresh *http.Response, stale *http.Response) {
	val, found := provider.cache.Get(core.MappingKeyPrefix + key)
	if !found {
		provider.logger.Sugar().Errorf("Impossible to get the mapping key %s in Otter", core.MappingKeyPrefix+key)

		return
	}

	fresh, stale, _ = core.MappingElection(provider, val, req, validator, provider.logger)

	return
}

// SetMultiLevel tries to store the key with the given value and update the mapping key to store metadata.
func (provider *Otter) SetMultiLevel(baseKey, variedKey string, value []byte, variedHeaders http.Header, etag string, duration time.Duration, realKey string) error {
	now := time.Now()

	compressed := new(bytes.Buffer)
	if _, err := lz4.NewWriter(compressed).ReadFrom(bytes.NewReader(value)); err != nil {
		provider.logger.Sugar().Errorf("Impossible to compress the key %s into Otter, %v", variedKey, err)

		return err
	}

	inserted := provider.cache.Set(variedKey, compressed.Bytes(), duration)
	if !inserted {
		provider.logger.Sugar().Errorf("Impossible to set value into Otter, too large for the cost function")

		return nil
	}

	mappingKey := core.MappingKeyPrefix + baseKey
	item, found := provider.cache.Get(mappingKey)

	if !found {
		provider.logger.Sugar().Errorf("Impossible to get the base key %s in Otter", mappingKey)

		return nil
	}

	val, e := core.MappingUpdater(variedKey, item, provider.logger, now, now.Add(duration), now.Add(duration+provider.stale), variedHeaders, etag, realKey)
	if e != nil {
		return e
	}

	provider.logger.Sugar().Debugf("Store the new mapping for the key %s in Otter", variedKey)
	// Used to calculate -(now * 2)
	negativeNow, _ := time.ParseDuration(fmt.Sprintf("-%d", time.Now().Nanosecond()*2))

	inserted = provider.cache.Set(mappingKey, val, negativeNow)
	if !inserted {
		provider.logger.Sugar().Errorf("Impossible to set value into Otter, too large for the cost function")

		return nil
	}

	return nil
}

// Set method will store the response in Otter provider.
func (provider *Otter) Set(key string, value []byte, duration time.Duration) error {
	inserted := provider.cache.Set(key, value, duration)
	if !inserted {
		provider.logger.Sugar().Errorf("Impossible to set value into Otter, too large for the cost function")
	}

	return nil
}

// Delete method will delete the response in Otter provider if exists corresponding to key param.
func (provider *Otter) Delete(key string) {
	provider.cache.Delete(key)
}

// DeleteMany method will delete the responses in Otter provider if exists corresponding to the regex key param.
func (provider *Otter) DeleteMany(key string) {
	rgKey, e := regexp.Compile(key)
	if e != nil {
		return
	}

	provider.cache.DeleteByFunc(func(k string, value []byte) bool {
		return rgKey.MatchString(k)
	})
}

// Init method will.
func (provider *Otter) Init() error {
	return nil
}

// Reset method will reset or close provider.
func (provider *Otter) Reset() error {
	provider.cache.Clear()

	return nil
}
