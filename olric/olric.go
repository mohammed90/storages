package olric

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/buraksezer/olric"
	"github.com/buraksezer/olric/config"
	"github.com/darkweak/storages/core"
	lz4 "github.com/pierrec/lz4/v4"
	"go.uber.org/zap"
)

// Olric provider type.
type Olric struct {
	*olric.ClusterClient
	dm            *sync.Pool
	stale         time.Duration
	logger        *zap.Logger
	addresses     []string
	reconnecting  bool
	configuration config.Client
}

// Factory function create new Olric instance.
func Factory(olricConfiguration core.CacheProvider, logger *zap.Logger, stale time.Duration) (core.Storer, error) {
	client, err := olric.NewClusterClient(strings.Split(olricConfiguration.URL, ","))
	if err != nil {
		logger.Sugar().Errorf("Impossible to connect to Olric, %v", err)
	}

	return &Olric{
		ClusterClient: client,
		dm:            nil,
		stale:         stale,
		logger:        logger,
		configuration: config.Client{},
		addresses:     strings.Split(olricConfiguration.URL, ","),
	}, nil
}

// Name returns the storer name.
func (provider *Olric) Name() string {
	return "OLRIC"
}

// Uuid returns an unique identifier.
func (provider *Olric) Uuid() string {
	return fmt.Sprintf("%s-%s", provider.addresses, provider.stale)
}

// ListKeys method returns the list of existing keys.
func (provider *Olric) ListKeys() []string {
	if provider.reconnecting {
		provider.logger.Sugar().Error("Impossible to list the olric keys while reconnecting.")

		return []string{}
	}

	dm := provider.dm.Get().(olric.DMap)
	defer provider.dm.Put(dm)

	records, err := dm.Scan(context.Background(), olric.Match("^"+core.MappingKeyPrefix))
	if err != nil {
		if !provider.reconnecting {
			go provider.Reconnect()
		}

		provider.logger.Sugar().Error("An error occurred while trying to list keys in Olric: %s\n", err)

		return []string{}
	}

	keys := []string{}

	for records.Next() {
		mapping, err := core.DecodeMapping(provider.Get(records.Key()))
		if err == nil {
			for _, v := range mapping.Mapping {
				keys = append(keys, v.RealKey)
			}
		}
	}
	records.Close()

	return keys
}

// MapKeys method returns the map of existing keys.
func (provider *Olric) MapKeys(prefix string) map[string]string {
	if provider.reconnecting {
		provider.logger.Sugar().Error("Impossible to list the olric keys while reconnecting.")

		return map[string]string{}
	}

	dm := provider.dm.Get().(olric.DMap)
	defer provider.dm.Put(dm)

	records, err := dm.Scan(context.Background())
	if err != nil {
		if !provider.reconnecting {
			go provider.Reconnect()
		}

		provider.logger.Sugar().Error("An error occurred while trying to list keys in Olric: %s\n", err)

		return map[string]string{}
	}

	keys := map[string]string{}

	for records.Next() {
		if strings.HasPrefix(records.Key(), prefix) {
			k, _ := strings.CutPrefix(records.Key(), prefix)
			keys[k] = string(provider.Get(records.Key()))
		}
	}

	records.Close()

	return keys
}

// GetMultiLevel tries to load the key and check if one of linked keys is a fresh/stale candidate.
func (provider *Olric) GetMultiLevel(key string, req *http.Request, validator *core.Revalidator) (fresh *http.Response, stale *http.Response) {
	dm := provider.dm.Get().(olric.DMap)
	defer provider.dm.Put(dm)
	res, e := dm.Get(context.Background(), key)

	if e != nil {
		return fresh, stale
	}

	val, _ := res.Byte()
	fresh, stale, _ = core.MappingElection(provider, val, req, validator, provider.logger)

	return fresh, stale
}

// SetMultiLevel tries to store the key with the given value and update the mapping key to store metadata.
func (provider *Olric) SetMultiLevel(baseKey, variedKey string, value []byte, variedHeaders http.Header, etag string, duration time.Duration, realKey string) error {
	now := time.Now()

	dmap := provider.dm.Get().(olric.DMap)
	defer provider.dm.Put(dmap)

	compressed := new(bytes.Buffer)

	if _, err := lz4.NewWriter(compressed).ReadFrom(bytes.NewReader(value)); err != nil {
		provider.logger.Sugar().Errorf("Impossible to compress the key %s into Olric, %v", variedKey, err)

		return err
	}

	if err := dmap.Put(context.Background(), variedKey, compressed.Bytes(), olric.EX(duration)); err != nil {
		provider.logger.Sugar().Errorf("Impossible to set value into Olric, %v", err)

		return err
	}

	mappingKey := core.MappingKeyPrefix + baseKey

	res, err := dmap.Get(context.Background(), mappingKey)
	if err != nil && !errors.Is(err, olric.ErrKeyNotFound) {
		provider.logger.Sugar().Errorf("Impossible to get the key %s Olric, %v", baseKey, err)

		return nil
	}

	val, err := res.Byte()
	if err != nil {
		provider.logger.Sugar().Errorf("Impossible to parse the key %s value as byte, %v", baseKey, err)

		return err
	}

	val, err = core.MappingUpdater(variedKey, val, provider.logger, now, now.Add(duration), now.Add(duration+provider.stale), variedHeaders, etag, realKey)
	if err != nil {
		return err
	}

	return provider.Set(mappingKey, val, time.Hour)
}

// Get method returns the populated response if exists, empty response then.
func (provider *Olric) Get(key string) []byte {
	if provider.reconnecting {
		provider.logger.Sugar().Error("Impossible to get the olric key while reconnecting.")

		return []byte{}
	}

	dm := provider.dm.Get().(olric.DMap)
	defer provider.dm.Put(dm)

	res, err := dm.Get(context.Background(), key)
	if err != nil {
		if !errors.Is(err, olric.ErrKeyNotFound) && !errors.Is(err, olric.ErrKeyTooLarge) && !provider.reconnecting {
			go provider.Reconnect()
		}

		return []byte{}
	}

	val, _ := res.Byte()

	return val
}

// Set method will store the response in Olric provider.
func (provider *Olric) Set(key string, value []byte, duration time.Duration) error {
	if provider.reconnecting {
		provider.logger.Sugar().Error("Impossible to set the olric value while reconnecting.")

		return errors.New("reconnecting error")
	}

	dm := provider.dm.Get().(olric.DMap)
	defer provider.dm.Put(dm)

	err := dm.Put(context.Background(), key, value, olric.EX(duration))
	if err != nil {
		if !provider.reconnecting {
			go provider.Reconnect()
		}

		provider.logger.Sugar().Errorf("Impossible to set value into Olric, %v", err)

		return err
	}

	return err
}

// Delete method will delete the response in Olric provider if exists corresponding to key param.
func (provider *Olric) Delete(key string) {
	if provider.reconnecting {
		provider.logger.Sugar().Error("Impossible to delete the olric key while reconnecting.")

		return
	}

	dm := provider.dm.Get().(olric.DMap)
	defer provider.dm.Put(dm)

	_, err := dm.Delete(context.Background(), key)
	if err != nil {
		provider.logger.Sugar().Errorf("Impossible to delete value into Olric, %v", err)
	}
}

// DeleteMany method will delete the responses in Olric provider if exists corresponding to the regex key param.
func (provider *Olric) DeleteMany(key string) {
	if provider.reconnecting {
		provider.logger.Sugar().Error("Impossible to delete the olric keys while reconnecting.")

		return
	}

	dmap := provider.dm.Get().(olric.DMap)
	defer provider.dm.Put(dmap)

	records, err := dmap.Scan(context.Background(), olric.Match(key))
	if err != nil {
		if !provider.reconnecting {
			go provider.Reconnect()
		}

		provider.logger.Sugar().Error("An error occurred while trying to list keys in Olric: %s\n", err)

		return
	}

	keys := []string{}
	for records.Next() {
		keys = append(keys, records.Key())
	}
	records.Close()

	_, _ = dmap.Delete(context.Background(), keys...)
}

// Init method will initialize Olric provider if needed.
func (provider *Olric) Init() error {
	dmap := sync.Pool{
		New: func() interface{} {
			dmap, _ := provider.ClusterClient.NewDMap("souin-map")

			return dmap
		},
	}

	provider.dm = &dmap

	return nil
}

// Reset method will reset or close provider.
func (provider *Olric) Reset() error {
	provider.ClusterClient.Close(context.Background())

	return nil
}

func (provider *Olric) Reconnect() {
	provider.reconnecting = true

	if c, err := olric.NewClusterClient(provider.addresses, olric.WithConfig(&provider.configuration)); err == nil && c != nil {
		provider.ClusterClient = c
		provider.reconnecting = false
	} else {
		time.Sleep(10 * time.Second)
		provider.Reconnect()
	}
}
