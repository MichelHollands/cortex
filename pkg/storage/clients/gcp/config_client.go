package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io/ioutil"
	"strings"

	alertStore "github.com/cortexproject/cortex/pkg/alertmanager/storage"
	"github.com/cortexproject/cortex/pkg/ruler/store"
	"github.com/cortexproject/cortex/pkg/util"

	gstorage "cloud.google.com/go/storage"
	"github.com/go-kit/kit/log/level"
	"github.com/golang/protobuf/proto"
	"github.com/prometheus/prometheus/pkg/rulefmt"
	"google.golang.org/api/iterator"
)

const (
	alertPrefix = "alerts/"
	rulePrefix  = "rules/"
)

var (
	errBadRuleGroup = errors.New("unable to decompose handle for rule object")
)

// GCSConfig is config for the GCS Chunk Client.
type GCSConfig struct {
	BucketName string `yaml:"bucket_name"`
}

// RegisterFlagsWithPrefix registers flags.
func (cfg *GCSConfig) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
	f.StringVar(&cfg.BucketName, prefix+"gcs.bucketname", "", "Name of GCS bucket to put chunks in.")
}

// GCSClient acts as a config backend. It is not safe to use concurrently when polling for rules.
// This is not an issue with the current scheduler architecture, but must be noted.
type GCSClient struct {
	client *gstorage.Client
	bucket *gstorage.BucketHandle
}

// NewGCSClient makes a new chunk.ObjectClient that writes chunks to GCS.
func NewGCSClient(ctx context.Context, cfg GCSConfig) (*GCSClient, error) {
	client, err := gstorage.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	return newGCSClient(cfg, client), nil
}

// newGCSClient makes a new chunk.ObjectClient that writes chunks to GCS.
func newGCSClient(cfg GCSConfig, client *gstorage.Client) *GCSClient {
	bucket := client.Bucket(cfg.BucketName)
	return &GCSClient{
		client: client,
		bucket: bucket,
	}
}

func (g *GCSClient) ListAlertConfigs(ctx context.Context) (map[string]alertStore.AlertConfig, error) {
	it := g.bucket.Objects(ctx, &gstorage.Query{
		Prefix: alertPrefix,
	})

	configs := map[string]alertStore.AlertConfig{}

	for {
		obj, err := it.Next()
		if err == iterator.Done {
			break
		}

		if err != nil {
			return nil, err
		}

		alertConfig, err := g.getAlertConfig(ctx, obj.Name)
		if err != nil {
			return nil, err
		}

		user := strings.TrimPrefix(obj.Name, alertPrefix)

		configs[user] = alertConfig
	}

	return configs, nil
}

func (g *GCSClient) getAlertConfig(ctx context.Context, obj string) (alertStore.AlertConfig, error) {
	reader, err := g.bucket.Object(obj).NewReader(ctx)
	if err == gstorage.ErrObjectNotExist {
		level.Debug(util.Logger).Log("msg", "object does not exist", "name", obj)
		return alertStore.AlertConfig{}, nil
	}
	if err != nil {
		return alertStore.AlertConfig{}, err
	}
	defer reader.Close()

	buf, err := ioutil.ReadAll(reader)
	if err != nil {
		return alertStore.AlertConfig{}, err
	}

	config := alertStore.AlertConfig{}
	err = json.Unmarshal(buf, &config)
	if err != nil {
		return alertStore.AlertConfig{}, err
	}

	return config, nil
}

// GetAlertConfig returns a specified users alertmanager configuration
func (g *GCSClient) GetAlertConfig(ctx context.Context, userID string) (alertStore.AlertConfig, error) {
	return g.getAlertConfig(ctx, alertPrefix+userID)
}

// SetAlertConfig sets a specified users alertmanager configuration
func (g *GCSClient) SetAlertConfig(ctx context.Context, userID string, cfg alertStore.AlertConfig) error {
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	objHandle := g.bucket.Object(alertPrefix + userID)

	writer := objHandle.NewWriter(ctx)
	if _, err := writer.Write(cfgBytes); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	return nil
}

// DeleteAlertConfig deletes a specified users alertmanager configuration
func (g *GCSClient) DeleteAlertConfig(ctx context.Context, userID string) error {
	err := g.bucket.Object(alertPrefix + userID).Delete(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (g *GCSClient) getAllRuleGroups(ctx context.Context, userID string) ([]store.RuleGroup, error) {
	it := g.bucket.Objects(ctx, &gstorage.Query{
		Prefix: generateRuleHandle(userID, "", ""),
	})

	rgs := []store.RuleGroup{}

	for {
		obj, err := it.Next()
		if err == iterator.Done {
			break
		}

		if err != nil {
			return []store.RuleGroup{}, err
		}

		rgProto, err := g.getRuleGroup(ctx, obj.Name)
		if err != nil {
			return []store.RuleGroup{}, err
		}

		rgs = append(rgs, store.ToRuleGroup(rgProto))
	}

	return rgs, nil
}

func (g *GCSClient) ListRuleGroups(ctx context.Context, options store.RuleStoreConditions) (store.RuleGroupList, error) {
	it := g.bucket.Objects(ctx, &gstorage.Query{
		Prefix: generateRuleHandle(options.UserID, options.Namespace, ""),
	})

	groups := []store.RuleGroup{}
	for {
		obj, err := it.Next()
		if err == iterator.Done {
			break
		}

		if err != nil {
			return nil, err
		}

		level.Debug(util.Logger).Log("msg", "listing rule group", "handle", obj.Name)

		rg, err := g.getRuleGroup(ctx, obj.Name)
		if err != nil {
			return nil, err
		}
		groups = append(groups, store.ToRuleGroup(rg))
	}
	return groups, nil
}

func (g *GCSClient) getRuleNamespace(ctx context.Context, userID string, namespace string) ([]*store.RuleGroupDesc, error) {
	it := g.bucket.Objects(ctx, &gstorage.Query{
		Prefix: generateRuleHandle(userID, namespace, ""),
	})

	groups := []*store.RuleGroupDesc{}

	for {
		obj, err := it.Next()
		if err == iterator.Done {
			break
		}

		if err != nil {
			return nil, err
		}

		rg, err := g.getRuleGroup(ctx, obj.Name)
		if err != nil {
			return nil, err
		}

		groups = append(groups, rg)
	}

	return groups, nil
}

func (g *GCSClient) GetRuleGroup(ctx context.Context, userID string, namespace string, grp string) (store.RuleGroup, error) {
	handle := generateRuleHandle(userID, namespace, grp)
	rg, err := g.getRuleGroup(ctx, handle)
	if err != nil {
		return nil, err
	}

	if rg == nil {
		return nil, store.ErrGroupNotFound
	}

	return store.ToRuleGroup(rg), nil
}

func (g *GCSClient) getRuleGroup(ctx context.Context, handle string) (*store.RuleGroupDesc, error) {
	reader, err := g.bucket.Object(handle).NewReader(ctx)
	if err == gstorage.ErrObjectNotExist {
		level.Debug(util.Logger).Log("msg", "rule group does not exist", "name", handle)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	buf, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	rg := &store.RuleGroupDesc{}

	err = proto.Unmarshal(buf, rg)
	if err != nil {
		return nil, err
	}

	return rg, nil
}

func (g *GCSClient) SetRuleGroup(ctx context.Context, userID string, namespace string, grp rulefmt.RuleGroup) error {
	rg := store.ToProto(userID, namespace, grp)
	rgBytes, err := proto.Marshal(&rg)
	if err != nil {
		return err
	}

	handle := generateRuleHandle(userID, namespace, grp.Name)
	objHandle := g.bucket.Object(handle)

	writer := objHandle.NewWriter(ctx)
	if _, err := writer.Write(rgBytes); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	return nil
}

func (g *GCSClient) DeleteRuleGroup(ctx context.Context, userID string, namespace string, group string) error {
	handle := generateRuleHandle(userID, namespace, group)
	err := g.bucket.Object(handle).Delete(ctx)
	if err != nil {
		return err
	}

	return nil
}

func generateRuleHandle(id, namespace, name string) string {
	if id == "" {
		return rulePrefix
	}
	prefix := rulePrefix + id + "/"
	if namespace == "" {
		return prefix
	}
	return prefix + namespace + "/" + name
}