package secrets

import (
	"context"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ResolveFromK8s loads key-value pairs from a Kubernetes Secret reference
// (format: "namespace/name"). Env vars with matching keys override Secret values.
func ResolveFromK8s(secretRef string, keys []string) (map[string]string, error) {
	namespace, name, err := parseSecretRef(secretRef)
	if err != nil {
		return nil, err
	}

	config, err := k8sConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("k8s clientset: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get secret %s/%s: %w", namespace, name, err)
	}

	result := make(map[string]string, len(keys))
	for _, k := range keys {
		result[k] = string(secret.Data[k])
	}

	// Env vars override K8s values
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			result[k] = v
		}
	}

	return result, nil
}

// ResolveFromEnv loads values for the given keys from environment variables.
func ResolveFromEnv(keys []string) map[string]string {
	result := make(map[string]string, len(keys))
	for _, k := range keys {
		result[k] = os.Getenv(k)
	}
	return result
}

// ResolveKeyFromConfigMap reads one key out of a ConfigMap reference (format:
// "namespace/name").
//
// It exists so a client that needs only a CA bundle can be granted `get` on a
// ConfigMap holding public material, instead of `get` on the Secret that also
// holds the API listener's PRIVATE KEY. Kubernetes RBAC cannot restrict a read
// to one key of an object, so pointing a client at the serving Secret hands it
// the key as well — which is why this path exists alongside ResolveFromK8s.
// Populate such a ConfigMap with cert-manager's trust-manager, or by hand.
func ResolveKeyFromConfigMap(ref, key string) (string, error) {
	namespace, name, err := parseSecretRef(ref)
	if err != nil {
		return "", err
	}

	config, err := k8sConfig()
	if err != nil {
		return "", fmt.Errorf("k8s config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("k8s clientset: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cm, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get configmap %s/%s: %w", namespace, name, err)
	}
	return cm.Data[key], nil
}

func parseSecretRef(ref string) (namespace, name string, err error) {
	for i, c := range ref {
		if c == '/' {
			return ref[:i], ref[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("secret ref must be \"namespace/name\", got %q", ref)
}

func k8sConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return nil, fmt.Errorf("no k8s config available: %w", err)
		}
	}
	return config, nil
}
