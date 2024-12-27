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
	"strings"
	"sync"

	ackclient "github.com/alibabacloud-go/cs-20151215/v5/client"
	ecs "github.com/alibabacloud-go/ecs-20140526/v4/client"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-alibabacloud/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-alibabacloud/pkg/providers/version"
)

type Provider interface {
	List(ctx context.Context, nodeClass *v1alpha1.ECSNodeClass) (Images, error)
}

type DefaultProvider struct {
	region    string
	ecsClient *ecs.Client
	ackClient *ackclient.Client

	sync.Mutex
	cache *cache.Cache

	versionProvider version.Provider
}

func NewDefaultProvider(region string, ecsClient *ecs.Client, ackClient *ackclient.Client,
	versionProvider version.Provider, cache *cache.Cache) *DefaultProvider {
	return &DefaultProvider{
		region:    region,
		ecsClient: ecsClient,
		ackClient: ackClient,

		cache:           cache,
		versionProvider: versionProvider,
	}
}

// List Get Returning a list of Images with its associated requirements
func (p *DefaultProvider) List(ctx context.Context, nodeClass *v1alpha1.ECSNodeClass) (Images, error) {
	p.Lock()
	defer p.Unlock()

	k8sVersion, err := p.versionProvider.Get(ctx)
	if err != nil {
		return nil, err
	}
	images, err := p.GetImages(k8sVersion, nodeClass)
	if err != nil {
		return nil, err
	}

	return images, nil
}

//nolint:gocyclo
func (p *DefaultProvider) GetImages(k8sVersion string, nodeClass *v1alpha1.ECSNodeClass) (Images, error) {
	hash, err := hashstructure.Hash(nodeClass.Spec.ImageSelectorTerms, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	if err != nil {
		return nil, err
	}
	if images, ok := p.cache.Get(fmt.Sprintf("%d", hash)); ok {
		// Ensure what's returned from this function is a deep-copy of Images so alterations
		// to the data don't affect the original
		return append(Images{}, images.(Images)...), nil
	}

	images := map[uint64]Image{}
	for _, selectorTerm := range nodeClass.Spec.ImageSelectorTerms {
		var ims Images
		var err error
		if selectorTerm.Alias != "" {
			ims, err = p.getImagesWithAlias(k8sVersion)
			if err != nil {
				return nil, err
			}
			imageFamily := GetImageFamily(selectorTerm.Alias)
			if imageFamily == nil {
				return nil, fmt.Errorf("unsupported image family %s", selectorTerm.Alias)
			}
			ims = imageFamily.ResolveImages(ims)
		} else {
			ims, err = p.getImagesWithID(selectorTerm.ID)
			if err != nil {
				return nil, err
			}
		}

		for _, im := range ims {
			reqsHash := lo.Must(hashstructure.Hash(im.Requirements.NodeSelectorRequirements(),
				hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true}))
			// So, this means, the further ahead, the higher the priority.
			if _, ok := images[reqsHash]; ok {
				continue
			}
			images[reqsHash] = im
		}
	}

	p.cache.SetDefault(fmt.Sprintf("%d", hash), Images(lo.Values(images)))
	return lo.Values(images), nil
}

//nolint:gocyclo
func (p *DefaultProvider) getImagesWithAlias(k8sVersion string) (Images, error) {
	// When query, the api ask to remove v from v1.6.0
	formatVersion := strings.TrimPrefix(k8sVersion, "v")
	req := &ackclient.DescribeKubernetesVersionMetadataRequest{
		Region:            tea.String(p.region),
		ClusterType:       tea.String("ManagedKubernetes"),
		KubernetesVersion: tea.String(formatVersion),
	}

	resp, err := p.ackClient.DescribeKubernetesVersionMetadata(req)
	if err != nil {
		return nil, fmt.Errorf("failed to describe k8s version metadata %w", err)
	}

	if resp == nil || resp.Body == nil || len(resp.Body) == 0 {
		return nil, nil
	}

	images := Images{}
	for _, image := range resp.Body[0].Images {
		imageID := tea.StringValue(image.ImageId)
		// not support i386
		arch, ok := v1alpha1.AlibabaCloudToKubeArchitectures[lo.FromPtr(image.Architecture)]
		if !ok {
			continue
		}
		requirement := scheduling.NewRequirement(
			corev1.LabelArchStable, corev1.NodeSelectorOpIn, arch)

		images = append(images, Image{
			Name:         tea.StringValue(image.ImageName),
			ImageID:      imageID,
			Requirements: scheduling.NewRequirements(requirement),
		})
	}

	return images, nil
}

func (p *DefaultProvider) getImagesWithID(id string) (Images, error) {
	req := &ecs.DescribeImagesRequest{
		RegionId:    tea.String(p.region),
		ImageId:     tea.String(id),
		ShowExpired: tea.Bool(true),
	}

	resp, err := p.ecsClient.DescribeImages(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get images through id %s", id)
	}

	if resp == nil || resp.Body == nil || resp.Body.Images == nil || len(resp.Body.Images.Image) == 0 {
		return nil, nil
	}

	images := Images{}
	for _, image := range resp.Body.Images.Image {
		arch, ok := v1alpha1.AlibabaCloudToKubeArchitectures[lo.FromPtr(image.Architecture)]
		if !ok {
			continue
		}
		requirement := scheduling.NewRequirement(
			corev1.LabelArchStable, corev1.NodeSelectorOpIn, arch)

		images = append(images, Image{
			Name:         tea.StringValue(image.ImageName),
			ImageID:      id,
			Requirements: scheduling.NewRequirements(requirement),
		})
	}

	return images, nil
}
