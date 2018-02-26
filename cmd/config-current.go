/*
 * Minio Cloud Storage, (C) 2016, 2017 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"errors"
	"fmt"
	"io/ioutil"
	"sync"

	"github.com/minio/minio/pkg/auth"
	"github.com/minio/minio/pkg/quick"
	"github.com/tidwall/gjson"
)

// Steps to move from version N to version N+1
// 1. Add new struct serverConfigVN+1 in config-versions.go
// 2. Set serverConfigVersion to "N+1"
// 3. Set serverConfig to serverConfigVN+1
// 4. Add new migration function (ex. func migrateVNToVN+1()) in config-migrate.go
// 5. Call migrateVNToVN+1() from migrateConfig() in config-migrate.go
// 6. Make changes in config-current_test.go for any test change

// Config version
const serverConfigVersion = "23"

type serverConfig = serverConfigV23

var (
	// globalServerConfig server config.
	globalServerConfig   *serverConfig
	globalServerConfigMu sync.RWMutex
	// Cache of mappings from accessKey to credentials.
	globalServerCredCache map[string]auth.Credentials
)

// GetVersion get current config version.
func (s *serverConfig) GetVersion() string {
	s.RLock()
	defer s.RUnlock()

	return s.Version
}

// SetRegion set a new region.
func (s *serverConfig) SetRegion(region string) {
	s.Lock()
	defer s.Unlock()

	// Save new region.
	s.Region = region
}

// GetRegion get current region.
func (s *serverConfig) GetRegion() string {
	s.RLock()
	defer s.RUnlock()

	return s.Region
}

// SetCredential sets new credentials and returns the previous credentials.
func (s *serverConfig) SetCredential(creds auth.Credentials) (prevCred auth.Credentials) {
	s.Lock()
	defer s.Unlock()

	// Save previous credential.
	prevCred = s.Credential

	// Set updated credential.
	s.Credential = creds

	// Return previous credential.
	return prevCred
}

// GetCredential gets the current credentials.
func (s *serverConfig) GetCredential() auth.Credentials {
	s.RLock()
	defer s.RUnlock()

	return s.Credential
}

// SetCredentialForBucket sets new credentials for a bucket and returns the previous credentials.
func (s *serverConfig) SetCredentialForBucket(bucket string, creds auth.Credentials) (prevCred auth.Credentials) {
	s.Lock()
	defer s.Unlock()

	if bucket == "" {
		prevCred = s.Credential
		s.Credential = creds
		return prevCred
	}

	// Save previous credentials.
	prevCred = s.Bucket[bucket]

	// If the credentials were valid, remove them from the cache.
	if prevCred.IsValid() {
		delete(globalServerCredCache, prevCred.AccessKey)

	} else {
		prevCred = s.Credential
	}

	// Set updated credentials officially and in the cache.
	s.Bucket[bucket] = creds;
	globalServerCredCache[creds.AccessKey] = creds

	// Return previous credentials.
	return prevCred
}

// GetCredentialForBucket get current credentials.
func (s *serverConfig) GetCredentialForBucket(bucket string) auth.Credentials {
	s.RLock()
	defer s.RUnlock()

	var cred auth.Credentials = s.Bucket[bucket]
	if !cred.IsValid() {
		cred = s.Credential
	}

	return cred
}

// GetBucketForKey returns the bucket name corresponding to a given access key,
// or the empty string if not found.
func (s *serverConfig) GetBucketForKey(key string) string {
	s.RLock()
	defer s.RUnlock()

	// Don't bother iterating if it's the master key.
	if key == s.Credential.AccessKey {
		return ""
	}

	// Go hunting for the bucket.
	for bucket, creds := range s.Bucket {
		if creds.AccessKey == key {
			return bucket
		}
	}

	return ""
}

// Attempt to find credentials for a given access key.
func (s *serverConfig) GetCredentialForKey(key string) auth.Credentials {
	s.RLock()
	defer s.RUnlock()

	var cred auth.Credentials = s.Credential
	if cred.AccessKey == key {
		return cred
	}

	// Try the cache for fast access.
	cred = globalServerCredCache[key]
	if cred.IsValid() && cred.AccessKey == key {
		return cred
	}

	// Go the slow way, looping through all the buckets.
	for _, cred = range s.Bucket {
		if cred.AccessKey == key {
			globalServerCredCache[cred.AccessKey] = cred
			return cred
		}
	}

	return auth.Credentials{}
}

// SetBrowser set if browser is enabled.
func (s *serverConfig) SetBrowser(b bool) {
	s.Lock()
	defer s.Unlock()

	// Set the new value.
	s.Browser = BrowserFlag(b)
}

func (s *serverConfig) SetStorageClass(standardClass, rrsClass storageClass) {
	s.Lock()
	defer s.Unlock()

	s.StorageClass.Standard = standardClass
	s.StorageClass.RRS = rrsClass
}

// GetStorageClass reads storage class fields from current config, parses and validates it.
// It returns the standard and reduced redundancy storage class struct
func (s *serverConfig) GetStorageClass() (storageClass, storageClass) {
	s.RLock()
	defer s.RUnlock()

	var err error
	// Storage Class from config.json is already parsed and stored in s.StorageClass
	// Now validate the storage class fields
	ssc := s.StorageClass.Standard
	rrsc := s.StorageClass.RRS

	if rrsc.Scheme != "" {
		err = validateRRSParity(rrsc.Parity, ssc.Parity)
		fatalIf(err, "Invalid value %s:%d set in config.json", rrsc.Scheme, rrsc.Parity)
		globalIsStorageClass = true
	}

	if ssc.Scheme != "" {
		err = validateSSParity(ssc.Parity, rrsc.Parity)
		fatalIf(err, "Invalid value %s:%d set in config.json", ssc.Scheme, ssc.Parity)
		globalIsStorageClass = true
	}

	return s.StorageClass.Standard, s.StorageClass.RRS
}

// GetCredentials get current credentials.
func (s *serverConfig) GetBrowser() bool {
	s.RLock()
	defer s.RUnlock()

	return bool(s.Browser)
}

// Save config.
func (s *serverConfig) Save() error {
	s.RLock()
	defer s.RUnlock()

	// Save config file.
	return quick.Save(getConfigFile(), s)
}

func newServerConfig() *serverConfig {
	srvCfg := &serverConfig{
		Version:      serverConfigVersion,
		Credential:   auth.MustGetNewCredentials(),
		Region:       globalMinioDefaultRegion,
		Browser:      true,
		StorageClass: storageClassConfig{},
		Notify:       &notifier{},
	}

	// Make sure to initialize notification configs.
	srvCfg.Notify.AMQP = make(map[string]amqpNotify)
	srvCfg.Notify.AMQP["1"] = amqpNotify{}
	srvCfg.Notify.MQTT = make(map[string]mqttNotify)
	srvCfg.Notify.MQTT["1"] = mqttNotify{}
	srvCfg.Notify.ElasticSearch = make(map[string]elasticSearchNotify)
	srvCfg.Notify.ElasticSearch["1"] = elasticSearchNotify{}
	srvCfg.Notify.Redis = make(map[string]redisNotify)
	srvCfg.Notify.Redis["1"] = redisNotify{}
	srvCfg.Notify.NATS = make(map[string]natsNotify)
	srvCfg.Notify.NATS["1"] = natsNotify{}
	srvCfg.Notify.PostgreSQL = make(map[string]postgreSQLNotify)
	srvCfg.Notify.PostgreSQL["1"] = postgreSQLNotify{}
	srvCfg.Notify.MySQL = make(map[string]mySQLNotify)
	srvCfg.Notify.MySQL["1"] = mySQLNotify{}
	srvCfg.Notify.Kafka = make(map[string]kafkaNotify)
	srvCfg.Notify.Kafka["1"] = kafkaNotify{}
	srvCfg.Notify.Webhook = make(map[string]webhookNotify)
	srvCfg.Notify.Webhook["1"] = webhookNotify{}
	srvCfg.Bucket = make(map[string]auth.Credentials)
	return srvCfg
}

// newConfig - initialize a new server config, saves env parameters if
// found, otherwise use default parameters
func newConfig() error {
	// Initialize server config.
	srvCfg := newServerConfig()

	// If env is set override the credentials from config file.
	if globalIsEnvCreds {
		srvCfg.SetCredential(globalActiveCred)
	}

	if globalIsEnvBrowser {
		srvCfg.SetBrowser(globalIsBrowserEnabled)
	}

	if globalIsEnvRegion {
		srvCfg.SetRegion(globalServerRegion)
	}

	if globalIsEnvDomainName {
		srvCfg.Domain = globalDomainName
	}

	if globalIsStorageClass {
		srvCfg.SetStorageClass(globalStandardStorageClass, globalRRStorageClass)
	}

	// hold the mutex lock before a new config is assigned.
	// Save the new config globally.
	// unlock the mutex.
	globalServerConfigMu.Lock()
	globalServerConfig = srvCfg
	globalServerCredCache = make(map[string]auth.Credentials)
	globalServerConfigMu.Unlock()

	// Save config into file.
	return globalServerConfig.Save()
}

// doCheckDupJSONKeys recursively detects duplicate json keys
func doCheckDupJSONKeys(key, value gjson.Result) error {
	// Key occurrences map of the current scope to count
	// if there is any duplicated json key.
	keysOcc := make(map[string]int)

	// Holds the found error
	var checkErr error

	// Iterate over keys in the current json scope
	value.ForEach(func(k, v gjson.Result) bool {
		// If current key is not null, check if its
		// value contains some duplicated keys.
		if k.Type != gjson.Null {
			keysOcc[k.String()]++
			checkErr = doCheckDupJSONKeys(k, v)
		}
		return checkErr == nil
	})

	// Check found err
	if checkErr != nil {
		return errors.New(key.String() + " => " + checkErr.Error())
	}

	// Check for duplicated keys
	for k, v := range keysOcc {
		if v > 1 {
			return errors.New(key.String() + " => `" + k + "` entry is duplicated")
		}
	}

	return nil
}

// Check recursively if a key is duplicated in the same json scope
// e.g.:
//  `{ "key" : { "key" ..` is accepted
//  `{ "key" : { "subkey" : "val1", "subkey": "val2" ..` throws subkey duplicated error
func checkDupJSONKeys(json string) error {
	// Parse config with gjson library
	config := gjson.Parse(json)

	// Create a fake rootKey since root json doesn't seem to have representation
	// in gjson library.
	rootKey := gjson.Result{Type: gjson.String, Str: minioConfigFile}

	// Check if loaded json contains any duplicated keys
	return doCheckDupJSONKeys(rootKey, config)
}

// getValidConfig - returns valid server configuration
func getValidConfig() (*serverConfig, error) {
	srvCfg := &serverConfig{
		Region:  globalMinioDefaultRegion,
		Browser: true,
	}

	configFile := getConfigFile()
	if _, err := quick.Load(configFile, srvCfg); err != nil {
		return nil, err
	}

	if srvCfg.Version != serverConfigVersion {
		return nil, fmt.Errorf("configuration version mismatch. Expected: ‘%s’, Got: ‘%s’", serverConfigVersion, srvCfg.Version)
	}

	// Load config file json and check for duplication json keys
	jsonBytes, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	if err = checkDupJSONKeys(string(jsonBytes)); err != nil {
		return nil, err
	}

	// Validate credential fields only when
	// they are not set via the environment

	// Error out if global is env credential is not set and config has invalid credential
	if !globalIsEnvCreds && !srvCfg.Credential.IsValid() {
		return nil, errors.New("invalid credential in config file " + configFile)
	}

	// Validate notify field
	if err = srvCfg.Notify.Validate(); err != nil {
		return nil, err
	}

	return srvCfg, nil
}

// loadConfig - loads a new config from disk, overrides params from env
// if found and valid
func loadConfig() error {
	srvCfg, err := getValidConfig()
	if err != nil {
		return err
	}

	// If env is set override the credentials from config file.
	if globalIsEnvCreds {
		srvCfg.SetCredential(globalActiveCred)
	}

	if globalIsEnvBrowser {
		srvCfg.SetBrowser(globalIsBrowserEnabled)
	}

	if globalIsEnvRegion {
		srvCfg.SetRegion(globalServerRegion)
	}

	if globalIsEnvDomainName {
		srvCfg.Domain = globalDomainName
	}

	if globalIsStorageClass {
		srvCfg.SetStorageClass(globalStandardStorageClass, globalRRStorageClass)
	}

	// hold the mutex lock before a new config is assigned.
	globalServerConfigMu.Lock()
	globalServerCredCache = make(map[string]auth.Credentials)
	globalServerConfig = srvCfg
	if !globalIsEnvCreds {
		globalActiveCred = globalServerConfig.GetCredential()
	}
	if !globalIsEnvBrowser {
		globalIsBrowserEnabled = globalServerConfig.GetBrowser()
	}
	if !globalIsEnvRegion {
		globalServerRegion = globalServerConfig.GetRegion()
	}
	if !globalIsEnvDomainName {
		globalDomainName = globalServerConfig.Domain
	}
	if !globalIsStorageClass {
		globalStandardStorageClass, globalRRStorageClass = globalServerConfig.GetStorageClass()
	}
	globalServerConfigMu.Unlock()

	return nil
}
