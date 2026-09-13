package k8s

import (
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// PodInformer interface for pod informer operations
type PodInformer interface {
	AddEventHandler(handler cache.ResourceEventHandler) error
	Run(stopCh <-chan struct{})
	HasSynced() bool
	GetStore() cache.Store
}

// CreatePodInformer creates a new pod informer
func (c *Client) CreatePodInformer() (PodInformer, error) {
	// Create informer factory with 30 second resync period
	informerFactory := informers.NewSharedInformerFactory(c.clientset, time.Second*30)

	// Create pod informer
	informer := informerFactory.Core().V1().Pods().Informer()

	return &podInformer{informer: informer}, nil
}

// podInformer implements PodInformer interface
type podInformer struct {
	informer cache.SharedIndexInformer
}

func (p *podInformer) AddEventHandler(handler cache.ResourceEventHandler) error {
	_, err := p.informer.AddEventHandler(handler)
	return err
}

func (p *podInformer) Run(stopCh <-chan struct{}) {
	p.informer.Run(stopCh)
}

func (p *podInformer) HasSynced() bool {
	return p.informer.HasSynced()
}

func (p *podInformer) GetStore() cache.Store {
	return p.informer.GetStore()
}
