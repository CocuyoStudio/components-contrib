/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package firestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/pubsub"
	jsoniter "github.com/json-iterator/go"
	"google.golang.org/api/option"

	"github.com/dapr/components-contrib/metadata"
	"github.com/dapr/components-contrib/state"
	"github.com/dapr/kit/logger"
)

const (
	defaultEntityKind = "DaprState"
	endpointKey       = "endpoint"
)

// Firestore State Store.
type Firestore struct {
	state.BulkStore

	client       *datastore.Client
	pubsubClient *pubsub.Client
	entityKind   string
	noIndex      bool
	logger       logger.Logger
}

type firestoreMetadata struct {
	Type                string `json:"type"`
	ProjectID           string `json:"project_id" mapstructure:"project_id"`
	PrivateKeyID        string `json:"private_key_id" mapstructure:"private_key_id"`
	PrivateKey          string `json:"private_key" mapstructure:"private_key"`
	ClientEmail         string `json:"client_email" mapstructure:"client_email"`
	ClientID            string `json:"client_id" mapstructure:"client_id"`
	AuthURI             string `json:"auth_uri" mapstructure:"auth_uri"`
	TokenURI            string `json:"token_uri" mapstructure:"token_uri"`
	AuthProviderCertURL string `json:"auth_provider_x509_cert_url" mapstructure:"auth_provider_x509_cert_url"`
	ClientCertURL       string `json:"client_x509_cert_url" mapstructure:"client_x509_cert_url"`
	EntityKind          string `json:"entity_kind" mapstructure:"entity_kind"`
	NoIndex             bool   `json:"-"`
	ConnectionEndpoint  string `json:"endpoint"`
}

type StateEntity struct {
	Value string
}

type StateEntityNoIndex struct {
	Value string `datastore:",noindex"`
}

func NewFirestoreStateStore(logger logger.Logger) state.Store {
	s := &Firestore{
		logger: logger,
	}
	s.BulkStore = state.NewDefaultBulkStore(s)
	return s
}

// Init does metadata and connection parsing.
func (f *Firestore) Init(ctx context.Context, metadata state.Metadata) error {
	meta, err := getFirestoreMetadata(metadata)
	if err != nil {
		return err
	}

	client, err := getGCPClient(ctx, meta, f.logger)
	if err != nil {
		return err
	}

	f.client = client
	f.entityKind = meta.EntityKind
	f.noIndex = meta.NoIndex

	return nil
}

// Features returns the features available in this state store.
func (f *Firestore) Features() []state.Feature {
	return nil
}

// Get retrieves state from Firestore with a key (Always strong consistency).
func (f *Firestore) Get(ctx context.Context, req *state.GetRequest) (*state.GetResponse, error) {
	key := req.Key

	entityKey := datastore.NameKey(f.entityKind, key, nil)
	var entity StateEntity
	err := f.client.Get(ctx, entityKey, &entity)

	if err != nil && !errors.Is(err, datastore.ErrNoSuchEntity) {
		return nil, err
	} else if errors.Is(err, datastore.ErrNoSuchEntity) {
		return &state.GetResponse{}, nil
	}

	return &state.GetResponse{
		Data: []byte(entity.Value),
	}, nil
}

// Set saves state into Firestore.
func (f *Firestore) Set(ctx context.Context, req *state.SetRequest) error {
	// localPubsubTopic(ctx, "SET")

	//f.addPubsubTopic(ctx)

	err := state.CheckRequestOptions(req.Options)
	if err != nil {
		return err
	}

	err = doPut(ctx, f.noIndex, f.entityKind, f.client, req)
	if err != nil {
		return err
	}

	return nil
}

func doPut(ctx context.Context, noIndex bool, entityKind string, client *datastore.Client, req *state.SetRequest) error {
	fmt.Printf("@@@ Firestore.doPut entityKind: %s\n\n", entityKind)
	var v string
	b, ok := req.Value.([]byte)
	if ok {
		v = string(b)
	} else {
		v, _ = jsoniter.MarshalToString(req.Value)
	}

	var entity interface{}
	if noIndex {
		entity = &StateEntityNoIndex{
			Value: v,
		}
	} else {
		entity = &StateEntity{
			Value: v,
		}
	}
	key := datastore.NameKey(entityKind, req.Key, nil)

	fmt.Printf("@@@ Firestore Put Key: %#v Entity: %#v\n\n", key, entity)
	_, err := client.Put(ctx, key, entity)

	if err != nil {
		fmt.Printf("@@@ Firestore Put err: %v\n\n", err)
		return err
	}
	return nil
}

// Delete performs a delete operation.
func (f *Firestore) Delete(ctx context.Context, req *state.DeleteRequest) error {
	key := datastore.NameKey(f.entityKind, req.Key, nil)

	err := f.client.Delete(ctx, key)
	if err != nil {
		return err
	}

	return nil
}

func (f *Firestore) GetComponentMetadata() (metadataInfo metadata.MetadataMap) {
	metadataStruct := firestoreMetadata{}
	metadata.GetMetadataInfoFromStructType(reflect.TypeOf(metadataStruct), &metadataInfo, metadata.StateStoreType)
	return
}

func getFirestoreMetadata(meta state.Metadata) (*firestoreMetadata, error) {
	m := firestoreMetadata{
		EntityKind: defaultEntityKind,
	}

	err := metadata.DecodeMetadata(meta.Properties, &m)
	if err != nil {
		return nil, err
	}

	requiredMetaProperties := []string{
		"project_id",
	}

	metadataMap := map[string]string{}
	bytes, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(bytes, &metadataMap)
	if err != nil {
		return nil, err
	}

	for _, k := range requiredMetaProperties {
		if val, ok := metadataMap[k]; !ok || len(val) < 1 {
			return nil, fmt.Errorf("error parsing required field: %s", k)
		}
	}

	if val, found := meta.Properties[endpointKey]; found && val != "" {
		m.ConnectionEndpoint = val
	}

	return &m, nil
}

func getGCPClient(ctx context.Context, metadata *firestoreMetadata, l logger.Logger) (*datastore.Client, error) {
	var gcpClient *datastore.Client
	var err error

	if metadata.PrivateKeyID != "" {
		var b []byte
		b, err = json.Marshal(metadata)
		if err != nil {
			return nil, err
		}

		opt := option.WithCredentialsJSON(b)
		gcpClient, err = datastore.NewClient(ctx, metadata.ProjectID, opt)
		if err != nil {
			return nil, err
		}
	} else {
		l.Debugf("Using implicit credentials for GCP")

		// The following allows the Google SDK to connect to
		// the GCP Datastore Emulator.
		// example: export DATASTORE_EMULATOR_HOST=localhost:8432
		// see: https://cloud.google.com/pubsub/docs/emulator#env
		if metadata.ConnectionEndpoint != "" {
			l.Debugf("setting GCP Datastore Emulator environment variable to 'DATASTORE_EMULATOR_HOST=%s'", metadata.ConnectionEndpoint)
			os.Setenv("DATASTORE_EMULATOR_HOST", metadata.ConnectionEndpoint)
		}
		gcpClient, err = datastore.NewClient(ctx, metadata.ProjectID)
		if err != nil {
			return nil, err
		}
		// req := &state.SetRequest{
		// 	Key:   "rob-key1",
		// 	Value: "rob-value1",
		// }
		// err = doPut(context.Background(), metadata.NoIndex, metadata.EntityKind, gcpClient, req)
		// if err != nil {
		// 	return nil, err
		// }
		key := datastore.NameKey(metadata.EntityKind, "nonexistingkey", nil)
		dst := &Nonexistenkind{}
		err = gcpClient.Get(context.Background(), key, &dst)
		if err != nil {
			fmt.Printf("@@@ getGCPClient error pinging EntityKind %q : %#v\n", metadata.EntityKind, err)
			return nil, err
		}

	}

	return gcpClient, nil
}

type Nonexistenkind struct {
	Nonexistingkey string
}

func pubsubClient(ctx context.Context, metadata *firestoreMetadata, l logger.Logger) (*pubsub.Client, error) {
	client, err := pubsub.NewClient(context.Background(), metadata.ProjectID)
	if err != nil {
		return nil, err
	}

	fmt.Printf("@@@ Firestore pubsubClient...\n\n")
	clientPubsubTopic(context.Background(), client, "PSC")
	return client, nil
}

func (f *Firestore) addPubsubTopic(ctx context.Context) {
	fmt.Printf("@@@ Firestore addPubsubTopic-2...\n\n")
	//ctx := context.Background()

	// Sets your Google Cloud Platform project ID.
	projectID := os.Getenv("GCP_PROJECT_ID")
	fmt.Printf("@@@ addPubsubTopic Project: %q...\n\n", projectID)

	// Creates a client.
	// client, err := pubsub.NewClient(ctx, projectID)
	// if err != nil {
	// 	log.Fatalf("Failed to create client: %v", err)
	// }
	// defer client.Close()

	// Sets the id for the new topic.
	topicID := os.Getenv("GCP_CERT_TEST_TOPIC") + os.Getenv("RANDOM")
	fmt.Printf("@@@ addPubsubTopic Topic: %q...\n\n", topicID)

	// Creates the new topic.
	topic, err := f.pubsubClient.CreateTopic(ctx, topicID)
	if err != nil {
		fmt.Printf("Failed to create topic: %v", err)
	} else {
		fmt.Printf("Topic %v created.\n", topic)
	}

}

func localPubsubTopic(ctx context.Context, prefix string) {
	fmt.Printf("@@@ Firestore localPubsubTopic...\n\n")
	//ctx := context.Background()

	// Sets your Google Cloud Platform project ID.
	projectID := os.Getenv("GCP_PROJECT_ID")
	fmt.Printf("@@@ addPubsubTopic Project: %q...\n\n", projectID)

	//Creates a client.
	client, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	// Sets the id for the new topic.
	topicID := os.Getenv("GCP_CERT_TEST_TOPIC") + os.Getenv("RANDOM") + prefix
	fmt.Printf("@@@ addPubsubTopic Topic: %q...\n\n", topicID)

	// Creates the new topic.
	topic, err := client.CreateTopic(ctx, topicID)
	if err != nil {
		fmt.Printf("Failed to create topic: %v", err)
	} else {
		fmt.Printf("Topic %v created.\n", topic)
	}

}

func clientPubsubTopic(ctx context.Context, client *pubsub.Client, prefix string) {
	fmt.Printf("@@@ Firestore clientPubsubTopic...\n\n")
	//ctx := context.Background()

	// Sets your Google Cloud Platform project ID.
	projectID := os.Getenv("GCP_PROJECT_ID")
	fmt.Printf("@@@ addPubsubTopic Project: %q...\n\n", projectID)

	// Sets the id for the new topic.
	topicID := os.Getenv("GCP_CERT_TEST_TOPIC") + os.Getenv("RANDOM") + prefix
	fmt.Printf("@@@ addPubsubTopic Topic: %q...\n\n", topicID)

	// Creates the new topic.
	topic, err := client.CreateTopic(ctx, topicID)
	if err != nil {
		fmt.Printf("Failed to create topic: %v", err)
	} else {
		fmt.Printf("Topic %v created.\n", topic)
	}

}
