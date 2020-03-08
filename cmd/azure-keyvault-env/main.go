// Copyright © 2019 Sparebanken Vest
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Note: Code is based on bank-vaults from Banzai Cloud
//       (https://github.com/banzaicloud/bank-vaults)

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SparebankenVest/azure-key-vault-to-kubernetes/pkg/akv2k8s/transformers"
	vault "github.com/SparebankenVest/azure-key-vault-to-kubernetes/pkg/azurekeyvault/client"
	akv "github.com/SparebankenVest/azure-key-vault-to-kubernetes/pkg/k8s/apis/azurekeyvault/v1alpha1"
	clientset "github.com/SparebankenVest/azure-key-vault-to-kubernetes/pkg/k8s/client/clientset/versioned"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

const (
	logPrefix    = "env-injector:"
	envLookupKey = "@azurekeyvault"
)

var logger *log.Entry

func formatLogger() {
	var logLevel string
	var ok bool

	log.SetFormatter(&log.TextFormatter{
		DisableColors: true,
		FullTimestamp: true,
	})

	logger = log.WithFields(log.Fields{
		"component":   "akv2k8s",
		"application": "env-injector",
	})

	if logLevel, ok = os.LookupEnv("ENV_INJECTOR_LOG_LEVEL"); !ok {
		logLevel = log.InfoLevel.String()
	}

	logrusLevel, err := log.ParseLevel(logLevel)
	if err != nil {
		log.Fatalf("%s Error setting log level: %s", logPrefix, err.Error())
	}
	log.SetLevel(logrusLevel)
}

// Retry will wait for a duration, retry n times, return if succeed or fails
// Thanks to Nick Stogner: https://upgear.io/blog/simple-golang-retry-function/
func retry(attempts int, sleep time.Duration, fn func() error) error {
	if err := fn(); err != nil {
		if s, ok := err.(stop); ok {
			// Return the original error for later checking
			return s.error
		}

		if attempts--; attempts > 0 {
			time.Sleep(sleep)
			return retry(attempts, 2*sleep, fn)
		}
		return err
	}
	return nil
}

type stop struct {
	error
}

func main() {
	var origCommand string
	var origArgs []string

	formatLogger()

	logger.Debugf("%s azure key vault env injector initializing", logPrefix)
	namespace := os.Getenv("ENV_INJECTOR_POD_NAMESPACE")
	if namespace == "" {
		logger.Fatalf("%s current namespace not provided in environment variable env_injector_pod_namespace", logPrefix)
	}

	logger = logger.WithFields(log.Fields{
		"namespace": namespace,
	})

	var err error
	retryTimes := 3
	waitTimeBetweenRetries := 3

	retryTimesEnv, ok := os.LookupEnv("ENV_INJECTOR_RETRIES")
	if ok {
		if retryTimes, err = strconv.Atoi(retryTimesEnv); err != nil {
			logger.Errorf("%s failed to convert ENV_INJECTOR_RETRIES env var into int, value was '%s', using default value of %d", logPrefix, retryTimesEnv, retryTimes)
		}
	}

	waitTimeBetweenRetriesEnv, ok := os.LookupEnv("ENV_INJECTOR_WAIT_BEFORE_RETRY")
	if ok {
		if waitTimeBetweenRetries, err := strconv.Atoi(retryTimesEnv); err != nil {
			logger.Errorf("%s failed to convert ENV_INJECTOR_WAIT_BEFORE_RETRY env var into int, value was '%s', using default value of %d", logPrefix, waitTimeBetweenRetriesEnv, waitTimeBetweenRetries)
		}
	}

	customAuth := strings.ToLower(os.Getenv("ENV_INJECTOR_CUSTOM_AUTH"))
	logger.Debugf("%s use custom auth: %s", logPrefix, customAuth)

	logger = logger.WithFields(log.Fields{
		"custom_auth": customAuth,
	})

	var creds *vault.AzureKeyVaultCredentials

	if customAuth == "true" {
		logger.Debugf("%s getting credentials for azure key vault using azure credentials supplied to pod", logPrefix)

		creds, err = vault.NewAzureKeyVaultCredentialsFromEnvironment()
		if err != nil {
			logger.Fatalf("%s failed to get credentials for azure key vault, error %+v", logPrefix, err)
		}
	} else {
		logger.Debugf("%s getting credentials for azure key vault using azure credentials from cloud config", logPrefix)
		creds, err = vault.NewAzureKeyVaultCredentialsFromCloudConfig("/azure-keyvault/azure.json")
		if err != nil {
			logger.Fatalf("%s failed to get credentials for azure key vault, error %+v", logPrefix, err)
		}
	}

	if len(os.Args) == 1 {
		logger.Fatalf("%s no command is given, currently vault-env can't determine the entrypoint (command), please specify it explicitly", logPrefix)
	} else {
		origCommand, err = exec.LookPath(os.Args[1])
		if err != nil {
			logger.Fatalf("%s binary not found: %s", logPrefix, err)
		}

		origArgs = os.Args[1:]

		logger.Infof("%s found original container command to be %s %s", logPrefix, origCommand, origArgs)
	}

	deleteSensitiveFiles()

	vaultService := vault.NewService(creds)

	logger.Debugf("%s reading azurekeyvaultsecret's referenced in env variables", logPrefix)
	cfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Fatalf("%s error building kubeconfig: %s", logPrefix, err.Error())
	}

	azureKeyVaultSecretClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("%s error building azurekeyvaultsecret clientset: %s", logPrefix, err.Error())
	}

	environ := os.Environ()

	for i, env := range environ {
		split := strings.SplitN(env, "=", 2)
		name := split[0]
		value := split[1]

		// e.g. my-akv-secret-name@azurekeyvault?some-sub-key
		if strings.Contains(value, envLookupKey) {
			// e.g. my-akv-secret-name?some-sub-key
			logger.Debugf("%s found env var '%s' to get azure key vault secret for", logPrefix, name)
			secretName := strings.Join(strings.Split(value, envLookupKey), "")

			if secretName == "" {
				logger.Fatalf("%s error extracting secret name from env variable '%s' with lookup value '%s' - not properly formatted", logPrefix, name, value)
			}

			var secretQuery string
			if query := strings.Split(secretName, "?"); len(query) > 1 {
				if len(query) > 2 {
					logger.Fatalf("%s error extracting secret query from '%s' - has multiple query elements defined with '?' - only one supported", logPrefix, secretName)
				}
				secretName = query[0]
				secretQuery = query[1]
				logger.Debugf("%s found query in env var '%s', '%s'", logPrefix, value, secretQuery)
			}

			logger.Debugf("%s getting azurekeyvaultsecret resource '%s' from kubernetes", logPrefix, secretName)
			keyVaultSecretSpec, err := azureKeyVaultSecretClient.AzurekeyvaultV1alpha1().AzureKeyVaultSecrets(namespace).Get(secretName, v1.GetOptions{})
			if err != nil {
				logger.Errorf("%s error getting azurekeyvaultsecret resource '%s', error: %s", logPrefix, secretName, err.Error())
				logger.Infof("%s will retry getting azurekeyvaultsecret resource up to %d times, waiting %d seconds between retries", logPrefix, retryTimes, waitTimeBetweenRetries)

				err = retry(retryTimes, time.Second*time.Duration(waitTimeBetweenRetries), func() error {
					keyVaultSecretSpec, err = azureKeyVaultSecretClient.AzurekeyvaultV1alpha1().AzureKeyVaultSecrets(namespace).Get(secretName, v1.GetOptions{})
					if err != nil {
						logger.Errorf("%s error getting azurekeyvaultsecret resource '%s', error: %s", logPrefix, secretName, err.Error())
						return err
					}
					logger.Infof("%s succeded getting azurekeyvaultsecret resource", logPrefix)
					return nil
				})
				if err != nil {
					logger.Fatalf("%s error getting azurekeyvaultsecret resource '%s', error: %s", logPrefix, secretName, err.Error())
				}
			}

			logger.Debugf("%s getting secret value for '%s' from azure key vault, to inject into env var %s", logPrefix, keyVaultSecretSpec.Spec.Vault.Object.Name, name)
			secret, err := getSecretFromKeyVault(keyVaultSecretSpec, secretQuery, vaultService)
			if err != nil {
				logger.Fatalf("%s failed to read secret '%s', error %+v", logPrefix, keyVaultSecretSpec.Spec.Vault.Object.Name, err)
			}

			if secret == "" {
				logger.Fatalf("%s secret not found in azure key vault: %s", logPrefix, keyVaultSecretSpec.Spec.Vault.Object.Name)
			} else {
				logger.Infof("%s secret %s injected into evn var %s for executable %s", logPrefix, keyVaultSecretSpec.Spec.Vault.Object.Name, name, origCommand)
				environ[i] = fmt.Sprintf("%s=%s", name, secret)
			}
		}
	}

	logger.Infof("%s starting process %s %v with secrets in env vars", logPrefix, origCommand, origArgs)
	err = syscall.Exec(origCommand, origArgs, environ)
	if err != nil {
		logger.Fatalf("%s failed to exec process '%s': %s", logPrefix, origCommand, err.Error())
	}

	logger.Infof("%s azure key vault env injector successfully injected env variables with secrets", logPrefix)
}

func deleteSensitiveFiles() {
	dirToRemove := "/azure-keyvault/"
	logger.Debugf("%s deleting files in directory '%s'", logPrefix, dirToRemove)
	err := clearDir(dirToRemove)
	if err != nil {
		logger.Errorf("%s error removing directory '%s' : %s", logPrefix, dirToRemove, err.Error())
	}
}

func clearDir(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return err
	}
	for _, file := range files {
		logger.Debugf("%s deleting file %s", logPrefix, file)
		err = os.Remove(file)
		if err != nil {
			logger.Errorf("%s failed to delete file %s, error %+v", logPrefix, file, err)
		}
	}
	return nil
}

func getSecretFromKeyVault(azureKeyVaultSecret *akv.AzureKeyVaultSecret, query string, vaultService vault.Service) (string, error) {
	var secretHandler EnvSecretHandler

	switch azureKeyVaultSecret.Spec.Vault.Object.Type {
	case akv.AzureKeyVaultObjectTypeSecret:
		transformator, err := transformers.CreateTransformator(&azureKeyVaultSecret.Spec.Output)
		if err != nil {
			return "", err
		}
		secretHandler = NewAzureKeyVaultSecretHandler(azureKeyVaultSecret, query, *transformator, vaultService)
	case akv.AzureKeyVaultObjectTypeCertificate:
		secretHandler = NewAzureKeyVaultCertificateHandler(azureKeyVaultSecret, query, vaultService)
	case akv.AzureKeyVaultObjectTypeKey:
		secretHandler = NewAzureKeyVaultKeyHandler(azureKeyVaultSecret, query, vaultService)
	case akv.AzureKeyVaultObjectTypeMultiKeyValueSecret:
		secretHandler = NewAzureKeyVaultMultiKeySecretHandler(azureKeyVaultSecret, query, vaultService)
	default:
		return "", fmt.Errorf("azure key vault object type '%s' not currently supported", azureKeyVaultSecret.Spec.Vault.Object.Type)
	}
	return secretHandler.Handle()
}
