/*
Copyright 2024 The CloudPilot AI Authors.

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

package imagefamily

import (
	"context"
	"fmt"
	"sync"

	ecs "github.com/alibabacloud-go/ecs-20140526/v4/client"
	util "github.com/alibabacloud-go/tea-utils/v2/service"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-alibabacloud/pkg/apis/v1alpha1"
)

var DefaultSystemDisk = v1alpha1.SystemDisk{
	// TODO: Change me, comprehensive ranking based on the pricing
	Categories: []string{"cloud", "cloud_ssd", "cloud_efficiency", "cloud_essd", "cloud_auto", "cloud_essd_entry"},
	Size:       tea.Int32(20),
}

// Options define the static launch template parameters
type Options struct {
	ClusterName     string
	ClusterEndpoint string
	// Level-triggered fields that may change out of sync.
	SecurityGroups []v1alpha1.SecurityGroup
	Tags           map[string]string
	Labels         map[string]string `hash:"ignore"`
	NodeClassName  string
}

// LaunchTemplate holds the dynamically generated launch template parameters
type LaunchTemplate struct {
	*Options
	UserData      string
	ImageID       string
	InstanceTypes []*cloudprovider.InstanceType `hash:"ignore"`
	SystemDisk    *v1alpha1.SystemDisk
	CapacityType  string
	// TODO: need more field, HttpTokens, RamRole, NetworkInterface, DataDisk, ...
}

type InstanceTypeAvailableSystemDisk struct {
	availableSystemDisk sets.Set[string]
	// todo: verify availability zone
	// availableZone sets.Set[string]
}

func newInstanceTypeAvailableSystemDisk() *InstanceTypeAvailableSystemDisk {
	return &InstanceTypeAvailableSystemDisk{}
}

func (s *InstanceTypeAvailableSystemDisk) AddAvailableSystemDisk(systemDisks ...string) {
	s.availableSystemDisk.Insert(systemDisks...)
}

func (s *InstanceTypeAvailableSystemDisk) Compatible(systemDisks []string) bool {
	for sdi := range systemDisks {
		if !s.availableSystemDisk.Has(systemDisks[sdi]) {
			return false
		}
	}

	return true
}

type Resolver interface {
	FilterInstanceTypesBySystemDisk(context.Context, *v1alpha1.ECSNodeClass, []*cloudprovider.InstanceType) []*cloudprovider.InstanceType
}

// DefaultResolver is able to fill-in dynamic launch template parameters
type DefaultResolver struct {
	sync.Mutex
	region string
	ecsapi *ecs.Client
	cache  *cache.Cache
}

// NewDefaultResolver constructs a new launch template DefaultResolver
func NewDefaultResolver(region string, ecsapi *ecs.Client, cache *cache.Cache) *DefaultResolver {
	return &DefaultResolver{
		region: region,
		ecsapi: ecsapi,
		cache:  cache,
	}
}

func GetImageFamily(family string) ImageFamily {
	switch family {
	case v1alpha1.ImageFamilyContainerOS:
		return &ContainerOS{}
	case v1alpha1.ImageFamilyAlibabaCloudLinux3:
		return &AlibabaCloudLinux3{}
	default:
		return nil
	}
}

// TODO: check system disk stock, currently only checking compatibility
func (r *DefaultResolver) FilterInstanceTypesBySystemDisk(ctx context.Context, nodeClass *v1alpha1.ECSNodeClass, instanceTypes []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	r.Lock()
	defer r.Unlock()

	if nodeClass.Spec.SystemDisk == nil || nodeClass.Spec.SystemDisk.Categories == nil {
		return instanceTypes
	}
	expectDiskCategories := nodeClass.Spec.SystemDisk.Categories

	// TODO: make following request parallel
	var result []*cloudprovider.InstanceType
	for i, instanceType := range instanceTypes {
		if availableSystemDisk, ok := r.cache.Get(instanceType.Name); ok {
			if availableSystemDisk.(*InstanceTypeAvailableSystemDisk).Compatible(expectDiskCategories) {
				result = append(result, instanceTypes[i])
			}
			continue
		}

		availableSystemDisk := newInstanceTypeAvailableSystemDisk()
		if err := r.describeAvailableSystemDisk(&ecs.DescribeAvailableResourceRequest{
			RegionId:            tea.String(r.region),
			DestinationResource: tea.String("SystemDisk"),
			InstanceType:        tea.String(instanceType.Name),
		}, func(resource *ecs.DescribeAvailableResourceResponseBodyAvailableZonesAvailableZoneAvailableResourcesAvailableResourceSupportedResourcesSupportedResource) {
			if *resource.Status == "Available" && *resource.Value != "" {
				availableSystemDisk.AddAvailableSystemDisk(*resource.Value)
			}
		}); err != nil {
			log.FromContext(ctx).Error(err, "describe available system disk failed")
			continue
		}

		if availableSystemDisk.Compatible(expectDiskCategories) {
			result = append(result, instanceTypes[i])
		} else {
			log.FromContext(ctx).V(1).Info("%s is not compatible with NodeClass %s SystemDisk %v", instanceType, nodeClass.Name, expectDiskCategories)
		}
		r.cache.SetDefault(instanceType.Name, availableSystemDisk)
	}
	return result
}

//nolint:gocyclo
func (r *DefaultResolver) describeAvailableSystemDisk(request *ecs.DescribeAvailableResourceRequest, process func(*ecs.DescribeAvailableResourceResponseBodyAvailableZonesAvailableZoneAvailableResourcesAvailableResourceSupportedResourcesSupportedResource)) error {
	runtime := &util.RuntimeOptions{}
	output, err := r.ecsapi.DescribeAvailableResourceWithOptions(request, runtime)
	if err != nil {
		return err
	} else if output == nil || output.Body == nil || output.Body.AvailableZones == nil {
		return fmt.Errorf("unexpected null value was returned")
	}
	for _, az := range output.Body.AvailableZones.AvailableZone {
		// todo: ClosedWithStock
		if *az.Status != "Available" || *az.StatusCategory != "WithStock" || az.AvailableResources == nil {
			continue
		}

		for _, ar := range az.AvailableResources.AvailableResource {
			if ar.SupportedResources == nil {
				continue
			}
			for _, sr := range ar.SupportedResources.SupportedResource {
				process(sr)
			}
		}
	}
	return nil
}
