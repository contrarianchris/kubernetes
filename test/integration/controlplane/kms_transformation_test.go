//go:build !windows
// +build !windows

/*
Copyright 2017 The Kubernetes Authors.

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

package controlplane

import (
	"bytes"
	"context"
	"crypto/aes"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/value"
	aestransformer "k8s.io/apiserver/pkg/storage/value/encrypt/aes"
	mock "k8s.io/apiserver/pkg/storage/value/encrypt/envelope/testing"
	kmsapi "k8s.io/apiserver/pkg/storage/value/encrypt/envelope/v1beta1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	dekKeySizeLen = 2
	kmsAPIVersion = "v1beta1"
)

type envelope struct {
	providerName string
	rawEnvelope  []byte
	plainTextDEK []byte
}

func (r envelope) prefix() string {
	return fmt.Sprintf("k8s:enc:kms:v1:%s:", r.providerName)
}

func (r envelope) prefixLen() int {
	return len(r.prefix())
}

func (r envelope) dekLen() int {
	// DEK's length is stored in the two bytes that follow the prefix.
	return int(binary.BigEndian.Uint16(r.rawEnvelope[r.prefixLen() : r.prefixLen()+dekKeySizeLen]))
}

func (r envelope) cipherTextDEK() []byte {
	return r.rawEnvelope[r.prefixLen()+dekKeySizeLen : r.prefixLen()+dekKeySizeLen+r.dekLen()]
}

func (r envelope) startOfPayload(providerName string) int {
	return r.prefixLen() + dekKeySizeLen + r.dekLen()
}

func (r envelope) cipherTextPayload() []byte {
	return r.rawEnvelope[r.startOfPayload(r.providerName):]
}

func (r envelope) plainTextPayload(secretETCDPath string) ([]byte, error) {
	block, err := aes.NewCipher(r.plainTextDEK)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize AES Cipher: %v", err)
	}
	// etcd path of the key is used as the authenticated context - need to pass it to decrypt
	ctx := context.Background()
	dataCtx := value.DefaultContext([]byte(secretETCDPath))
	aescbcTransformer := aestransformer.NewCBCTransformer(block)
	plainSecret, _, err := aescbcTransformer.TransformFromStorage(ctx, r.cipherTextPayload(), dataCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to transform from storage via AESCBC, err: %v", err)
	}

	return plainSecret, nil
}

// TestKMSProvider is an integration test between KubeAPI, ETCD and KMS Plugin
// Concretely, this test verifies the following integration contracts:
// 1. Raw records in ETCD that were processed by KMS Provider should be prefixed with k8s:enc:kms:v1:grpc-kms-provider-name:
// 2. Data Encryption Key (DEK) should be generated by envelopeTransformer and passed to KMS gRPC Plugin
// 3. KMS gRPC Plugin should encrypt the DEK with a Key Encryption Key (KEK) and pass it back to envelopeTransformer
// 4. The cipherTextPayload (ex. Secret) should be encrypted via AES CBC transform
// 5. Prefix-EncryptedDEK-EncryptedPayload structure should be deposited to ETCD
func TestKMSProvider(t *testing.T) {
	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: kms-provider
       cachesize: 1000
       endpoint: unix:///@kms-provider.sock
`

	providerName := "kms-provider"
	pluginMock, err := mock.NewBase64Plugin("@kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMS Plugin: %v", err)
	}

	go pluginMock.Start()
	if err := mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	defer pluginMock.CleanUp()

	test, err := newTransformTest(t, encryptionConfig)
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	defer test.cleanUp()

	test.secret, err = test.createSecret(testSecret, testNamespace)
	if err != nil {
		t.Fatalf("Failed to create test secret, error: %v", err)
	}

	// Since Data Encryption Key (DEK) is randomly generated (per encryption operation), we need to ask KMS Mock for it.
	plainTextDEK := pluginMock.LastEncryptRequest()

	secretETCDPath := test.getETCDPath()
	rawEnvelope, err := test.getRawSecretFromETCD()
	if err != nil {
		t.Fatalf("failed to read %s from etcd: %v", secretETCDPath, err)
	}
	envelope := envelope{
		providerName: providerName,
		rawEnvelope:  rawEnvelope,
		plainTextDEK: plainTextDEK,
	}

	wantPrefix := "k8s:enc:kms:v1:kms-provider:"
	if !bytes.HasPrefix(rawEnvelope, []byte(wantPrefix)) {
		t.Fatalf("expected secret to be prefixed with %s, but got %s", wantPrefix, rawEnvelope)
	}

	decryptResponse, err := pluginMock.Decrypt(context.Background(), &kmsapi.DecryptRequest{Version: kmsAPIVersion, Cipher: envelope.cipherTextDEK()})
	if err != nil {
		t.Fatalf("failed to decrypt DEK, %v", err)
	}
	dekPlainAsWouldBeSeenByETCD := decryptResponse.Plain

	if !bytes.Equal(plainTextDEK, dekPlainAsWouldBeSeenByETCD) {
		t.Fatalf("expected plainTextDEK %v to be passed to KMS Plugin, but got %s",
			plainTextDEK, dekPlainAsWouldBeSeenByETCD)
	}

	plainSecret, err := envelope.plainTextPayload(secretETCDPath)
	if err != nil {
		t.Fatalf("failed to transform from storage via AESCBC, err: %v", err)
	}

	if !strings.Contains(string(plainSecret), secretVal) {
		t.Fatalf("expected %q after decryption, but got %q", secretVal, string(plainSecret))
	}

	// Secrets should be un-enveloped on direct reads from Kube API Server.
	s, err := test.restClient.CoreV1().Secrets(testNamespace).Get(context.TODO(), testSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get Secret from %s, err: %v", testNamespace, err)
	}
	if secretVal != string(s.Data[secretKey]) {
		t.Fatalf("expected %s from KubeAPI, but got %s", secretVal, string(s.Data[secretKey]))
	}
}

func TestKMSHealthz(t *testing.T) {
	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       name: provider-1
       endpoint: unix:///@kms-provider-1.sock
    - kms:
       name: provider-2
       endpoint: unix:///@kms-provider-2.sock
`

	pluginMock1, err := mock.NewBase64Plugin("@kms-provider-1.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMS Plugin #1: %v", err)
	}

	if err := pluginMock1.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock1.CleanUp()
	if err := mock.WaitForBase64PluginToBeUp(pluginMock1); err != nil {
		t.Fatalf("Failed to start plugin #1, err: %v", err)
	}

	pluginMock2, err := mock.NewBase64Plugin("@kms-provider-2.sock")
	if err != nil {
		t.Fatalf("Failed to create mock of KMS Plugin #2: err: %v", err)
	}
	if err := pluginMock2.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock2.CleanUp()
	if err := mock.WaitForBase64PluginToBeUp(pluginMock2); err != nil {
		t.Fatalf("Failed to start KMS Plugin #2: err: %v", err)
	}

	test, err := newTransformTest(t, encryptionConfig)
	if err != nil {
		t.Fatalf("Failed to start kube-apiserver, error: %v", err)
	}
	defer test.cleanUp()

	// Name of the healthz check is calculated based on a constant "kms-provider-" + position of the
	// provider in the config.

	// Stage 1 - Since all kms-plugins are guaranteed to be up, healthz checks for:
	// healthz/kms-provider-0 and /healthz/kms-provider-1 should be OK.
	mustBeHealthy(t, "kms-provider-0", test.kubeAPIServer.ClientConfig)
	mustBeHealthy(t, "kms-provider-1", test.kubeAPIServer.ClientConfig)

	// Stage 2 - kms-plugin for provider-1 is down. Therefore, expect the health check for provider-1
	// to fail, but provider-2 should still be OK
	pluginMock1.EnterFailedState()
	mustBeUnHealthy(t, "kms-provider-0", test.kubeAPIServer.ClientConfig)
	mustBeHealthy(t, "kms-provider-1", test.kubeAPIServer.ClientConfig)
	pluginMock1.ExitFailedState()

	// Stage 3 - kms-plugin for provider-1 is now up. Therefore, expect the health check for provider-1
	// to succeed now, but provider-2 is now down.
	// Need to sleep since health check chases responses for 3 seconds.
	pluginMock2.EnterFailedState()
	mustBeHealthy(t, "kms-provider-0", test.kubeAPIServer.ClientConfig)
	mustBeUnHealthy(t, "kms-provider-1", test.kubeAPIServer.ClientConfig)
}

func mustBeHealthy(t *testing.T, checkName string, clientConfig *rest.Config) {
	t.Helper()
	var restErr error
	pollErr := wait.PollImmediate(2*time.Second, wait.ForeverTestTimeout, func() (bool, error) {
		status, err := getHealthz(checkName, clientConfig)
		restErr = err
		if err != nil {
			return false, err
		}
		return status == http.StatusOK, nil
	})

	if pollErr == wait.ErrWaitTimeout {
		t.Fatalf("failed to get the expected healthz status of OK for check: %s, error: %v, debug inner error: %v", checkName, pollErr, restErr)
	}
}

func mustBeUnHealthy(t *testing.T, checkName string, clientConfig *rest.Config) {
	t.Helper()
	var restErr error
	pollErr := wait.PollImmediate(2*time.Second, wait.ForeverTestTimeout, func() (bool, error) {
		status, err := getHealthz(checkName, clientConfig)
		restErr = err
		if err != nil {
			return false, err
		}
		return status != http.StatusOK, nil
	})

	if pollErr == wait.ErrWaitTimeout {
		t.Fatalf("failed to get the expected healthz status of !OK for check: %s, error: %v, debug inner error: %v", checkName, pollErr, restErr)
	}
}

func getHealthz(checkName string, clientConfig *rest.Config) (int, error) {
	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return 0, fmt.Errorf("failed to create a client: %v", err)
	}

	result := client.CoreV1().RESTClient().Get().AbsPath(fmt.Sprintf("/healthz/%v", checkName)).Do(context.TODO())
	status := 0
	result.StatusCode(&status)
	return status, nil
}
