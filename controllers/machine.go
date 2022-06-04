/*
Copyright 2022 The Tinkerbell Authors.

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

package controllers

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	tinkv1 "github.com/tinkerbell/tink/pkg/apis/core/v1alpha1"

	infrastructurev1 "github.com/tinkerbell/cluster-api-provider-tinkerbell/api/v1beta1"
	"github.com/tinkerbell/cluster-api-provider-tinkerbell/internal/templates"
)

const providerIDPlaceholder = "PROVIDER_ID"

type machineReconcileContext struct {
	*baseMachineReconcileContext

	machine              *clusterv1.Machine
	tinkerbellCluster    *infrastructurev1.TinkerbellCluster
	bootstrapCloudConfig string
}

// ErrHardwareMissingDiskConfiguration is returned when the referenced hardware is missing
// disk configuration.
var ErrHardwareMissingDiskConfiguration = fmt.Errorf("disk configuration is required")

// MachineCreator is a subset of tinkerbellCluster used by machineReconcileContext.
type MachineCreator interface {
	// Template related functions.
	CreateTemplate(ctx context.Context, name, data string) (string, error)

	// Workflow related functions.
	CreateWorkflow(ctx context.Context, templateID, hardware string) (string, error)

	// Hardware related functions.
	HardwareIDByIP(ctx context.Context, ip string) (string, error)
	GetHardwareIP(ctx context.Context, id string) (string, error)
	NextAvailableHardwareID(ctx context.Context) (string, error)
	HardwareAvailable(ctx context.Context, id string) (bool, error)
}

func (mrc *machineReconcileContext) addFinalizer() error {
	controllerutil.AddFinalizer(mrc.tinkerbellMachine, infrastructurev1.MachineFinalizer)

	if err := mrc.patch(); err != nil {
		return fmt.Errorf("patching TinkerbellMachine object with finalizer: %w", err)
	}

	return nil
}

func (mrc *machineReconcileContext) ensureDependencies() error {
	hardware, err := mrc.ensureHardware()
	if err != nil {
		return fmt.Errorf("ensuring hardware: %w", err)
	}

	if err := mrc.ensureTemplate(hardware); err != nil {
		return fmt.Errorf("ensuring template: %w", err)
	}

	if err := mrc.ensureWorkflow(hardware); err != nil {
		return fmt.Errorf("ensuring workflow: %w", err)
	}

	return nil
}

func (mrc *machineReconcileContext) markAsReady() error {
	mrc.tinkerbellMachine.Status.Ready = true

	if err := mrc.patch(); err != nil {
		return fmt.Errorf("patching machine with ready status: %w", err)
	}

	return nil
}

func (mrc *machineReconcileContext) Reconcile() error {
	// To make sure we do not create orphaned objects.
	if err := mrc.addFinalizer(); err != nil {
		return fmt.Errorf("adding finalizer: %w", err)
	}

	if err := mrc.ensureDependencies(); err != nil {
		return fmt.Errorf("ensuring machine dependencies: %w", err)
	}

	if err := mrc.markAsReady(); err != nil {
		return fmt.Errorf("marking machine as ready: %w", err)
	}

	if err := mrc.updateHardwareState(); err != nil {
		return fmt.Errorf("error setting hardware state: %w", err)
	}

	return nil
}

func (mrc *machineReconcileContext) templateExists() (bool, error) {
	namespacedName := types.NamespacedName{
		Name:      mrc.tinkerbellMachine.Name,
		Namespace: mrc.tinkerbellMachine.Namespace,
	}

	err := mrc.client.Get(mrc.ctx, namespacedName, &tinkv1.Template{})
	if err == nil {
		return true, nil
	}

	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("checking if template exists: %w", err)
	}

	return false, nil
}

func (mrc *machineReconcileContext) imageURL() (string, error) {
	imageLookupFormat := mrc.tinkerbellMachine.Spec.ImageLookupFormat
	if imageLookupFormat == "" {
		imageLookupFormat = mrc.tinkerbellCluster.Spec.ImageLookupFormat
	}

	imageLookupBaseRegistry := mrc.tinkerbellMachine.Spec.ImageLookupBaseRegistry
	if imageLookupBaseRegistry == "" {
		imageLookupBaseRegistry = mrc.tinkerbellCluster.Spec.ImageLookupBaseRegistry
	}

	imageLookupOSDistro := mrc.tinkerbellMachine.Spec.ImageLookupOSDistro
	if imageLookupOSDistro == "" {
		imageLookupOSDistro = mrc.tinkerbellCluster.Spec.ImageLookupOSDistro
	}

	imageLookupOSVersion := mrc.tinkerbellMachine.Spec.ImageLookupOSVersion
	if imageLookupOSVersion == "" {
		imageLookupOSVersion = mrc.tinkerbellCluster.Spec.ImageLookupOSVersion
	}

	return imageURL(
		imageLookupFormat,
		imageLookupBaseRegistry,
		imageLookupOSDistro,
		imageLookupOSVersion,
		*mrc.machine.Spec.Version,
	)
}

func (mrc *machineReconcileContext) createTemplate(hardware *tinkv1.Hardware) error {
	if len(hardware.Spec.Disks) < 1 {
		return ErrHardwareMissingDiskConfiguration
	}

	templateData := mrc.tinkerbellMachine.Spec.TemplateOverride
	if templateData == "" {
		targetDisk := hardware.Spec.Disks[0].Device
		targetDevice := firstPartitionFromDevice(targetDisk)

		imageURL, err := mrc.imageURL()
		if err != nil {
			return fmt.Errorf("failed to generate imageURL: %w", err)
		}

		metadataIP := os.Getenv("TINKERBELL_IP")
		if metadataIP == "" {
			metadataIP = "192.168.1.1"
		}

		metadataURL := fmt.Sprintf("http://%s:50061", metadataIP)

		workflowTemplate := templates.WorkflowTemplate{
			Name:          mrc.tinkerbellMachine.Name,
			MetadataURL:   metadataURL,
			ImageURL:      imageURL,
			DestDisk:      targetDisk,
			DestPartition: targetDevice,
		}

		templateData, err = workflowTemplate.Render()
		if err != nil {
			return fmt.Errorf("rendering template: %w", err)
		}
	}

	templateObject := &tinkv1.Template{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mrc.tinkerbellMachine.Name,
			Namespace: mrc.tinkerbellMachine.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "TinkerbellMachine",
					Name:       mrc.tinkerbellMachine.Name,
					UID:        mrc.tinkerbellMachine.ObjectMeta.UID,
				},
			},
		},
		Spec: tinkv1.TemplateSpec{
			Data: &templateData,
		},
	}

	if err := mrc.client.Create(mrc.ctx, templateObject); err != nil {
		return fmt.Errorf("creating Tinkerbell template: %w", err)
	}

	return nil
}

func firstPartitionFromDevice(device string) string {
	nvmeDevice := regexp.MustCompile(`^/dev/nvme\d+n\d+$`)
	emmcDevice := regexp.MustCompile(`^/dev/mmcblk\d+$`)

	switch {
	case nvmeDevice.MatchString(device), emmcDevice.MatchString(device):
		return fmt.Sprintf("%sp1", device)
	default:
		return fmt.Sprintf("%s1", device)
	}
}

func (mrc *machineReconcileContext) ensureTemplate(hardware *tinkv1.Hardware) error {
	// TODO: should this reconccile the template instead of just ensuring it exists?
	templateExists, err := mrc.templateExists()
	if err != nil {
		return fmt.Errorf("checking if Template exists: %w", err)
	}

	if templateExists {
		return nil
	}

	mrc.Log().Info("template for machine does not exist, creating")

	return mrc.createTemplate(hardware)
}

func (mrc *machineReconcileContext) takeHardwareOwnership(hardware *tinkv1.Hardware) error {
	if len(hardware.ObjectMeta.Labels) == 0 {
		hardware.ObjectMeta.Labels = map[string]string{}
	}

	hardware.ObjectMeta.Labels[HardwareOwnerNameLabel] = mrc.tinkerbellMachine.Name
	hardware.ObjectMeta.Labels[HardwareOwnerNamespaceLabel] = mrc.tinkerbellMachine.Namespace

	// Add finalizer to hardware as well to make sure we release it before Machine object is removed.
	controllerutil.AddFinalizer(hardware, infrastructurev1.MachineFinalizer)

	if err := mrc.client.Update(mrc.ctx, hardware); err != nil {
		return fmt.Errorf("patching Hardware object: %w", err)
	}

	return nil
}

func (mrc *machineReconcileContext) setStatus(hardware *tinkv1.Hardware) error {
	if hardware == nil {
		hardware = &tinkv1.Hardware{}

		namespacedName := types.NamespacedName{
			Name:      mrc.tinkerbellMachine.Spec.HardwareName,
			Namespace: mrc.tinkerbellMachine.Namespace,
		}

		if err := mrc.client.Get(mrc.ctx, namespacedName, hardware); err != nil {
			return fmt.Errorf("getting Hardware: %w", err)
		}
	}

	ip, err := hardwareIP(hardware)
	if err != nil {
		return fmt.Errorf("extracting Hardware IP address: %w", err)
	}

	mrc.tinkerbellMachine.Status.Addresses = []corev1.NodeAddress{
		{
			Type:    corev1.NodeInternalIP,
			Address: ip,
		},
	}

	return mrc.patch()
}

func (mrc *machineReconcileContext) ensureHardwareUserData(hardware *tinkv1.Hardware, providerID string) error {
	userData := strings.ReplaceAll(mrc.bootstrapCloudConfig, providerIDPlaceholder, providerID)

	if hardware.Spec.UserData == nil || *hardware.Spec.UserData != userData {
		patchHelper, err := patch.NewHelper(hardware, mrc.client)
		if err != nil {
			return fmt.Errorf("initializing patch helper for selected hardware: %w", err)
		}

		hardware.Spec.UserData = &userData

		if err := patchHelper.Patch(mrc.ctx, hardware); err != nil {
			return fmt.Errorf("patching Hardware object: %w", err)
		}
	}

	return nil
}

func (mrc *machineReconcileContext) ensureHardware() (*tinkv1.Hardware, error) {
	hardware, err := mrc.hardwareForMachine()
	if err != nil {
		return nil, fmt.Errorf("getting hardware: %w", err)
	}

	if err := mrc.takeHardwareOwnership(hardware); err != nil {
		return nil, fmt.Errorf("taking Hardware ownership: %w", err)
	}

	if mrc.tinkerbellMachine.Spec.HardwareName == "" {
		mrc.log.Info("Selected Hardware for machine", "Hardware name", hardware.Name)
	}

	mrc.tinkerbellMachine.Spec.HardwareName = hardware.Name
	mrc.tinkerbellMachine.Spec.ProviderID = fmt.Sprintf("tinkerbell://%s/%s", hardware.Namespace, hardware.Name)

	if err := mrc.ensureHardwareUserData(hardware, mrc.tinkerbellMachine.Spec.ProviderID); err != nil {
		return nil, fmt.Errorf("ensuring Hardware user data: %w", err)
	}

	return hardware, mrc.setStatus(hardware)
}

func (mrc *machineReconcileContext) hardwareForMachine() (*tinkv1.Hardware, error) {
	// first query for hardware that's already assigned
	if hardware, err := mrc.assignedHardware(); err != nil {
		return nil, err
	} else if hardware != nil {
		return hardware, nil
	}

	// then fallback to searching for new hardware
	hardwareSelector := mrc.tinkerbellMachine.Spec.HardwareAffinity.DeepCopy()
	if hardwareSelector == nil {
		hardwareSelector = &infrastructurev1.HardwareAffinity{}
	}
	// if no terms are specified, we create an empty one to ensure we always query for non-selected hardware
	if len(hardwareSelector.Required) == 0 {
		hardwareSelector.Required = append(hardwareSelector.Required, infrastructurev1.HardwareAffinityTerm{})
	}

	var matchingHardware []tinkv1.Hardware

	// OR all of the required terms by selecting each individually, we could end up with duplicates in matchingHardware
	// but it doesn't matter
	for i := range hardwareSelector.Required {
		var matched tinkv1.HardwareList

		// add a selector for unselected hardware
		hardwareSelector.Required[i].LabelSelector.MatchExpressions = append(
			hardwareSelector.Required[i].LabelSelector.MatchExpressions,
			metav1.LabelSelectorRequirement{
				Key:      HardwareOwnerNameLabel,
				Operator: metav1.LabelSelectorOpDoesNotExist,
			})

		selector, err := metav1.LabelSelectorAsSelector(&hardwareSelector.Required[i].LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("converting label selector: %w", err)
		}

		if err := mrc.client.List(mrc.ctx, &matched, &client.ListOptions{LabelSelector: selector}); err != nil {
			return nil, fmt.Errorf("listing hardware without owner: %w", err)
		}

		matchingHardware = append(matchingHardware, matched.Items...)
	}

	// finally sort by our preferred affinity terms
	cmp, err := byHardwareAffinity(matchingHardware, hardwareSelector.Preferred)
	if err != nil {
		return nil, fmt.Errorf("sorting hardware by preference: %w", err)
	}

	sort.Slice(matchingHardware, cmp)

	if len(matchingHardware) > 0 {
		return &matchingHardware[0], nil
	}
	// nothing was found
	return nil, ErrNoHardwareAvailable
}

// assignedHardware returns hardware that is already assigned. In the event of no hardware being assigned, it returns
// nil, nil.
func (mrc *machineReconcileContext) assignedHardware() (*tinkv1.Hardware, error) {
	var selectedHardware tinkv1.HardwareList
	if err := mrc.client.List(mrc.ctx, &selectedHardware, client.MatchingLabels{
		HardwareOwnerNameLabel:      mrc.tinkerbellMachine.Name,
		HardwareOwnerNamespaceLabel: mrc.tinkerbellMachine.Namespace,
	}); err != nil {
		return nil, fmt.Errorf("listing hardware with owner: %w", err)
	}

	if len(selectedHardware.Items) > 0 {
		return &selectedHardware.Items[0], nil
	}

	return nil, nil
}

//nolint:lll
func byHardwareAffinity(hardware []tinkv1.Hardware, preferred []infrastructurev1.WeightedHardwareAffinityTerm) (func(i int, j int) bool, error) {
	scores := map[client.ObjectKey]int32{}
	// compute scores for each item based on the preferred term weightss
	for _, term := range preferred {
		selector, err := metav1.LabelSelectorAsSelector(&term.HardwareAffinityTerm.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("constructing label selector: %w", err)
		}

		for i := range hardware {
			hw := &hardware[i]
			if selector.Matches(labels.Set(hw.Labels)) {
				scores[client.ObjectKeyFromObject(hw)] = term.Weight
			}
		}
	}

	return func(i, j int) bool {
		lhsScore := scores[client.ObjectKeyFromObject(&hardware[i])]
		rhsScore := scores[client.ObjectKeyFromObject(&hardware[j])]
		// sort by score in descending order
		if lhsScore > rhsScore {
			return true
		} else if lhsScore < rhsScore {
			return false
		}

		// just give a consistent ordering so we predictably pick one if scores are equal
		if hardware[i].Namespace != hardware[j].Namespace {
			return hardware[i].Namespace < hardware[j].Namespace
		}

		return hardware[i].Name < hardware[j].Name
	}, nil
}

func (mrc *machineReconcileContext) workflowExists() (bool, error) {
	namespacedName := types.NamespacedName{
		Name:      mrc.tinkerbellMachine.Name,
		Namespace: mrc.tinkerbellMachine.Namespace,
	}

	err := mrc.client.Get(mrc.ctx, namespacedName, &tinkv1.Workflow{})
	if err == nil {
		return true, nil
	}

	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("checking if workflow exists: %w", err)
	}

	return false, nil
}

func (mrc *machineReconcileContext) createWorkflow(hardware *tinkv1.Hardware) error {
	workflow := &tinkv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mrc.tinkerbellMachine.Name,
			Namespace: mrc.tinkerbellMachine.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
					Kind:       "TinkerbellMachine",
					Name:       mrc.tinkerbellMachine.Name,
					UID:        mrc.tinkerbellMachine.ObjectMeta.UID,
				},
			},
		},
		Spec: tinkv1.WorkflowSpec{
			TemplateRef: mrc.tinkerbellMachine.Name,
			HardwareMap: map[string]string{"device_1": hardware.Spec.Metadata.Instance.ID},
		},
	}

	if err := mrc.client.Create(mrc.ctx, workflow); err != nil {
		return fmt.Errorf("creating workflow: %w", err)
	}

	return nil
}

func (mrc *machineReconcileContext) ensureWorkflow(hardware *tinkv1.Hardware) error {
	workflowExists, err := mrc.workflowExists()
	if err != nil {
		return fmt.Errorf("checking if workflow exists: %w", err)
	}

	if workflowExists {
		return nil
	}

	mrc.log.Info("Workflow does not exist, creating")

	return mrc.createWorkflow(hardware)
}

type image struct {
	BaseRegistry      string
	OSDistro          string
	OSVersion         string
	KubernetesVersion string
}

func imageURL(imageFormat, baseRegistry, osDistro, osVersion, kubernetesVersion string) (string, error) {
	imageParams := image{
		BaseRegistry:      baseRegistry,
		OSDistro:          strings.ToLower(osDistro),
		OSVersion:         strings.ReplaceAll(osVersion, ".", ""),
		KubernetesVersion: kubernetesVersion,
	}

	var buf bytes.Buffer

	template, err := template.New("image").Parse(imageFormat)
	if err != nil {
		return "", fmt.Errorf("failed to create template from string %q: %w", imageFormat, err)
	}

	if err := template.Execute(&buf, imageParams); err != nil {
		return "", fmt.Errorf("failed to populate template %q: %w", imageFormat, err)
	}

	return buf.String(), nil
}
