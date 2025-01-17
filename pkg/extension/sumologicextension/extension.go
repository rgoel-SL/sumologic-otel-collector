// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sumologicextension

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/sumologicextension/api"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
)

type SumologicExtension struct {
	collectorName    string
	baseUrl          string
	httpClient       *http.Client
	conf             *Config
	logger           *zap.Logger
	credentialsStore CredentialsStore
	hashKey          string
	registrationInfo api.OpenRegisterResponsePayload
	closeChan        chan struct{}
	closeOnce        sync.Once
}

const (
	heartbeatUrl                  = "/api/v1/collector/heartbeat"
	registerUrl                   = "/api/v1/collector/register"
	collectorCredentialsDirectory = ".sumologic-otel-collector/"
)

const (
	DefaultHeartbeatInterval = 15 * time.Second
)

func newSumologicExtension(conf *Config, logger *zap.Logger) (*SumologicExtension, error) {
	if conf.Credentials.AccessID == "" || conf.Credentials.AccessKey == "" {
		return nil, errors.New("access_key and/or access_id not provided")
	}
	collectorName := ""
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	credentialsStore := localFsCredentialsStore{
		collectorCredentialsDirectory: conf.CollectorCredentialsDirectory,
		logger:                        logger,
	}
	if conf.CollectorName == "" {
		key := createHashKey(conf)
		// If collector name is not set by the user check if the collector was restarted
		if !credentialsStore.Check(key) {
			// If credentials file is not stored on filesystem generate collector name
			collectorName = fmt.Sprintf("%s-%s", hostname, uuid.New())
		}
	} else {
		collectorName = conf.CollectorName
	}
	if conf.HeartBeatInterval <= 0 {
		conf.HeartBeatInterval = DefaultHeartbeatInterval
	}
	hashKey := createHashKey(conf)

	return &SumologicExtension{
		collectorName:    collectorName,
		baseUrl:          strings.TrimSuffix(conf.ApiBaseUrl, "/"),
		conf:             conf,
		logger:           logger,
		hashKey:          hashKey,
		credentialsStore: credentialsStore,
		closeChan:        make(chan struct{}),
	}, nil
}

func createHashKey(conf *Config) string {
	return fmt.Sprintf("%s%s%s", conf.CollectorName, conf.Credentials.AccessID, conf.Credentials.AccessKey)
}

func (se *SumologicExtension) Start(ctx context.Context, host component.Host) error {
	var colCreds CollectorCredentials
	var err error
	if se.credentialsStore.Check(se.hashKey) {
		colCreds, err = se.credentialsStore.Get(se.hashKey)
		if err != nil {
			return err
		}
		se.collectorName = colCreds.CollectorName
		if !se.conf.Clobber {
			se.logger.Info("Found stored credentials, skipping registration")
		} else {
			se.logger.Info("Locally stored credentials found, but clobber flag is set: re-registering the collector")
			if colCreds, err = se.registerCollector(ctx, se.collectorName); err != nil {
				return err
			}
			if err := se.credentialsStore.Store(se.hashKey, colCreds); err != nil {
				se.logger.Error("Unable to store collector credentials",
					zap.Error(err),
				)
			}
		}
	} else {
		se.logger.Info("Locally stored credentials not found, registering the collector")
		if colCreds, err = se.registerCollector(ctx, se.collectorName); err != nil {
			return err
		}
		if err := se.credentialsStore.Store(se.hashKey, colCreds); err != nil {
			se.logger.Error("Unable to store collector credentials",
				zap.Error(err),
			)
		}
	}

	// TODO: change this to get the collector name as returned by the API.
	se.logger = se.logger.With(zap.String("collector_name", se.collectorName))

	se.registrationInfo = colCreds.Credentials

	se.httpClient, err = se.conf.HTTPClientSettings.ToClient(host.GetExtensions())
	if err != nil {
		return fmt.Errorf("couldn't create HTTP client: %w", err)
	}

	// Set the transport so that all requests from se.httpClient will contain
	// the collector credentials.
	rt, err := se.RoundTripper(se.httpClient.Transport)
	if err != nil {
		return fmt.Errorf("couldn't create HTTP client transport: %w", err)
	}
	se.httpClient.Transport = rt

	go se.heartbeatLoop()

	return nil
}

// Shutdown is invoked during service shutdown.
func (se *SumologicExtension) Shutdown(ctx context.Context) error {
	se.closeOnce.Do(func() { close(se.closeChan) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// registerCollector registers the collector using registration API and returns
// the obtained collector credentials.
func (se *SumologicExtension) registerCollector(ctx context.Context, collectorName string) (CollectorCredentials, error) {
	baseUrl := strings.TrimSuffix(se.conf.ApiBaseUrl, "/")
	u, err := url.Parse(baseUrl)
	if err != nil {
		return CollectorCredentials{}, err
	}
	u.Path = registerUrl

	// TODO: just plain hostname or we want to add some custom logic when setting
	// hostname in request?
	hostname, err := os.Hostname()
	if err != nil {
		return CollectorCredentials{}, fmt.Errorf("cannot get hostname: %w", err)
	}

	var buff bytes.Buffer
	if err = json.NewEncoder(&buff).Encode(api.OpenRegisterRequestPayload{
		CollectorName: collectorName,
		Description:   se.conf.CollectorDescription,
		Category:      se.conf.CollectorCategory,
		Fields:        se.conf.CollectorFields,
		Hostname:      hostname,
		Ephemeral:     se.conf.Ephemeral,
		Clobber:       se.conf.Clobber,
		TimeZone:      se.conf.TimeZone,
	}); err != nil {
		return CollectorCredentials{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &buff)
	if err != nil {
		return CollectorCredentials{}, err
	}

	addClientCredentials(req,
		credentials{
			AccessID:  se.conf.Credentials.AccessID,
			AccessKey: se.conf.Credentials.AccessKey,
		},
	)
	addJSONHeaders(req)

	se.logger.Info("Calling register API", zap.String("URL", u.String()))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return CollectorCredentials{}, fmt.Errorf("failed to register the collector: %w", err)
	}

	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 400 {
		var buff bytes.Buffer
		if _, err := io.Copy(&buff, res.Body); err != nil {
			return CollectorCredentials{}, fmt.Errorf(
				"failed to copy collector registration response body, status code: %d, err: %w",
				res.StatusCode, err,
			)
		}
		se.logger.Debug("Collector registration failed",
			zap.Int("status_code", res.StatusCode),
			zap.String("response", buff.String()),
		)
		return CollectorCredentials{}, fmt.Errorf(
			"failed to register the collector, got HTTP status code: %d",
			res.StatusCode,
		)
	}

	var resp api.OpenRegisterResponsePayload
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return CollectorCredentials{}, err
	}

	se.logger.Info("Collector registered",
		zap.String("CollectorID", resp.CollectorId),
		zap.Any("response", resp),
	)

	return CollectorCredentials{
		// TODO: When registration API will return registered name use it instead of collectorName
		CollectorName: collectorName,
		Credentials:   resp,
	}, nil
}

func (se *SumologicExtension) heartbeatLoop() {
	if se.registrationInfo.CollectorCredentialId == "" || se.registrationInfo.CollectorCredentialKey == "" {
		se.logger.Error("Collector not registered, cannot send heartbeat")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// When the close channel is closed ...
		<-se.closeChan
		// ... cancel the ongoing heartbeat request.
		cancel()
	}()

	se.logger.Info("Heartbeat API initialized. Starting sending hearbeat requests")
	timer := time.NewTimer(se.conf.HeartBeatInterval)
	for {
		select {
		case <-se.closeChan:
			se.logger.Info("Heartbeat sender turned off")
			return
		default:
			if err := se.sendHeartbeat(ctx); err != nil {
				se.logger.Error("Heartbeat error", zap.Error(err))
			} else {
				se.logger.Debug("Heartbeat sent")
			}

			select {
			case <-timer.C:
				timer.Stop()
				timer.Reset(se.conf.HeartBeatInterval)
			case <-se.closeChan:
			}

		}
	}
}

func (se *SumologicExtension) sendHeartbeat(ctx context.Context) error {
	u, err := url.Parse(se.baseUrl + heartbeatUrl)
	if err != nil {
		return fmt.Errorf("unable to parse heartbeat URL %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return fmt.Errorf("unable to create HTTP request %w", err)
	}

	addJSONHeaders(req)
	res, err := se.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("unable to send HTTP request: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 204 {
		var buff bytes.Buffer
		if _, err := io.Copy(&buff, res.Body); err != nil {
			return fmt.Errorf(
				"failed to copy collector heartbeat response body, status code: %d, err: %w",
				res.StatusCode, err,
			)
		}
		return fmt.Errorf(
			"collector heartbeat request failed, status code: %d, body: %s",
			res.StatusCode, buff.String(),
		)
	}
	return nil

}

func (se *SumologicExtension) ComponentID() string {
	return se.conf.ExtensionSettings.ID().String()
}

func (se *SumologicExtension) CollectorID() string {
	return se.registrationInfo.CollectorId
}

func (se *SumologicExtension) BaseUrl() string {
	return se.baseUrl
}

// Implement [1] in order for this extension to be used as custom exporter
// authenticator.
//
// [1]: https://github.com/open-telemetry/opentelemetry-collector/blob/2e84285efc665798d76773b9901727e8836e9d8f/config/configauth/clientauth.go#L34-L39
func (se *SumologicExtension) RoundTripper(base http.RoundTripper) (http.RoundTripper, error) {
	return roundTripper{
		collectorCredentialId:  se.registrationInfo.CollectorCredentialId,
		collectorCredentialKey: se.registrationInfo.CollectorCredentialKey,
		base:                   base,
	}, nil
}

type roundTripper struct {
	collectorCredentialId  string
	collectorCredentialKey string
	base                   http.RoundTripper
}

func (rt roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	addCollectorCredentials(req, rt.collectorCredentialId, rt.collectorCredentialKey)
	return rt.base.RoundTrip(req)
}

func addCollectorCredentials(req *http.Request, collectorCredentialId string, collectorCredentialKey string) {
	token := base64.StdEncoding.EncodeToString(
		[]byte(collectorCredentialId + ":" + collectorCredentialKey),
	)

	req.Header.Add("Authorization", "Basic "+token)
}

func addClientCredentials(req *http.Request, credentials credentials) {
	token := base64.StdEncoding.EncodeToString(
		[]byte(credentials.AccessID + ":" + credentials.AccessKey),
	)

	req.Header.Add("Authorization", "Basic "+token)
}
