/**
 * Copyright 2025 IBM Corp.
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

// Package watcher ...
package watcher

import (
	"flag"
	"os"
	"strings"
	"time"

	uid "github.com/gofrs/uuid"
	"go.uber.org/zap/zapcore"

	"github.com/golang/glog"

	"github.com/IBM/ibmcloud-volume-interface/config"
	"github.com/IBM/ibmcloud-volume-interface/lib/provider"
	iks_vpc_provider "github.com/IBM/ibmcloud-volume-vpc/iks/provider"
	cloudprovider "github.com/IBM/ibmcloud-volume-vpc/pkg/ibmcloudprovider"

	"go.uber.org/zap"
	"golang.org/x/net/context"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
)

// PVWatcher to watch  pv creation and add taggs
type PVWatcher struct {
	logger          *zap.Logger
	kclient         kubernetes.Interface
	config          *config.Config
	provisionerName string
	recorder        record.EventRecorder
	cloudProvider   cloudprovider.CloudProviderInterface
}

const (
	//IbmCloudGtAPIEndpoint ...
	IbmCloudGtAPIEndpoint = "IBMCLOUD_GT_API_ENDPOINT"
	//ReclaimPolicyTag ...
	ReclaimPolicyTag = "reclaimpolicy:"
	//NameSpaceTag ...
	NameSpaceTag = "namespace:"
	//StorageClassTag ...
	StorageClassTag = "storageclass:"
	//PVCNameTag ...
	PVCNameTag = "pvc:"
	//PVNameTag ...
	PVNameTag = "pv:"
	//VolumeCRN ...
	VolumeCRN = "volumeCRN"
	//ProvisionerTag ...
	ProvisionerTag = "provisioner:"

	//VolumeStatus ...
	VolumeStatus = "status"
	//VolumeStatusCreated ...
	VolumeStatusCreated = "created"
	//VolumeStatusDeleted ...
	VolumeStatusDeleted = "deleted"
	//VolumeUpdateEventReason ...
	VolumeUpdateEventReason = "VolumeMetaDataSaved"
	//VolumeUpdateEventSuccess ...
	VolumeUpdateEventSuccess = "Success"

	// VolumeIDLabel ...
	VolumeIDLabel = "volumeId"

	// VolumeCRNLabel ...
	VolumeCRNLabel = "volumeCRN"

	// ClusterIDLabel ...
	ClusterIDLabel = "clusterID"

	// IOPSLabel ...
	IOPSLabel = "iops"

	// ZoneLabel ...
	ZoneLabel = "zone"

	// GiB in bytes
	GiB = 1024 * 1024 * 1024
)

// VolumeTypeMap ...
var VolumeTypeMap = map[string]string{}

var master = flag.String(
	"master",
	"",
	"Master URL to build a client config from. Either this or kubeconfig needs to be set if the provisioner is being run out of cluster.",
)
var kubeconfig = flag.String(
	"kubeconfig",
	"",
	"Absolute path to the kubeconfig file. Either this or master needs to be set if the provisioner is being run out of cluster.",
)

// New creates the Watcher instance
func New(logger *zap.Logger, provisionerName string, volumeType string, cloudProvider cloudprovider.CloudProviderInterface) *PVWatcher {
	var restConfig *rest.Config
	var err error
	// Register provider
	VolumeTypeMap[provisionerName] = volumeType

	restConfig, err = clientcmd.BuildConfigFromFlags(*master, *kubeconfig)
	if err != nil {
		logger.Fatal("Failed to create config:", zap.Error(err))
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logger.Fatal("Failed to create client:", zap.Error(err))
	}
	iksPodName := os.Getenv("POD_NAME")

	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(glog.Infof)
	eventInterface := clientset.CoreV1().Events("")
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: eventInterface})
	pvw := &PVWatcher{
		logger:          logger,
		config:          cloudProvider.GetConfig(),
		provisionerName: provisionerName,
		kclient:         clientset,
		cloudProvider:   cloudProvider,
		recorder:        broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: iksPodName}),
	}
	return pvw
}

// Start start pv watcher
func (pvw *PVWatcher) Start() {
	watchlist := cache.NewListWatchFromClient(pvw.kclient.CoreV1().RESTClient(), "persistentvolumes", "", fields.Everything())
	_, controller := cache.NewInformer(watchlist, &v1.PersistentVolume{}, time.Second*0,
		cache.FilteringResourceEventHandler{
			Handler: cache.ResourceEventHandlerFuncs{
				UpdateFunc: pvw.updateVolume,
			},
			FilterFunc: pvw.filter,
		},
	)
	pvw.logger.Info("PVWatcher starting")
	stopch := wait.NeverStop
	go controller.Run(stopch)
	pvw.logger.Info("PVWatcher started")
	<-stopch
}

func (pvw *PVWatcher) updateVolume(oldobj, obj interface{}) {
	// Run as non-blocking thread to allow parallel processing of volumes
	go func() {
		var oldStatus v1.PersistentVolumePhase
		var newStatus v1.PersistentVolumePhase
		ctxLogger, requestID := GetContextLogger(context.Background(), false)
		// panic-recovery function that avoid watcher thread to stop because of unexexpected error
		defer func() {
			if r := recover(); r != nil {
				ctxLogger.Error("Recovered from panic in pvwatcher", zap.Stack("stack"), zap.String("requestID", requestID))
			}
		}()

		ctxLogger.Info("Entry updateVolume()", zap.Reflect("obj", obj), zap.Reflect("oldobj", oldobj))
		newpv, _ := obj.(*v1.PersistentVolume)
		//If there is no change to status , capacity or iops we can skip the updateVolume call.
		if oldobj != nil {
			oldpv, _ := oldobj.(*v1.PersistentVolume)
			oldCapacity := oldpv.Spec.Capacity[v1.ResourceStorage]
			capacity := newpv.Spec.Capacity[v1.ResourceStorage]
			iops := newpv.Spec.CSI.VolumeAttributes[IOPSLabel]
			oldiops := oldpv.Spec.CSI.VolumeAttributes[IOPSLabel]
			newStatus = newpv.Status.Phase
			oldStatus = oldpv.Status.Phase
			if (newStatus == oldStatus) && (oldCapacity.Value() == capacity.Value()) && (oldiops == iops) {
				ctxLogger.Info("Skipping update Volume as there is no change in status , capacity and iops")
				return
			}
		}

		session, err := pvw.cloudProvider.GetProviderSession(context.Background(), ctxLogger)
		if session != nil {
			iksVpc, ok := session.(*iks_vpc_provider.IksVpcSession)

			if !ok {
				ctxLogger.Error("Failed to get the IKS-VPC session, Try to restart the CSI driver controller POD")
				return
			}

			volume := pvw.getVolumeFromPV(newpv, ctxLogger)
			// Updating metadata for the volume
			ctxLogger.Info("Updating metadata for the volume", zap.Reflect("volume", volume))
			err := iksVpc.UpdateVolume(volume)
			if err != nil {
				ctxLogger.Warn("Failed to update volume metadata", zap.Error(err))
				pvw.recorder.Event(newpv, v1.EventTypeWarning, VolumeUpdateEventReason, err.Error())
			}

			//Lets invoke the VPC IaaS update Volume only if there is status change and new status is bound state.
			//This will be true only when PVC is first time created
			if newStatus != oldStatus && newStatus == v1.VolumeBound {
				ctxLogger.Info("Updating tags from VPC IaaS")
				err = iksVpc.VPCSession.UpdateVolume(volume)
				if err != nil {
					ctxLogger.Warn("Failed to update volume with tags from VPC IaaS", zap.Error(err))
					pvw.recorder.Event(newpv, v1.EventTypeWarning, VolumeUpdateEventReason, err.Error())
				} else {
					pvw.recorder.Event(newpv, v1.EventTypeNormal, VolumeUpdateEventReason, VolumeUpdateEventSuccess)
					ctxLogger.Warn("Volume Metadata saved successfully")
				}
			} else {
				ctxLogger.Info("Skipping Updating tags from VPC IaaS as there is no change in tags")
			}
		}
		ctxLogger.Info("Exit updateVolume()", zap.Error(err))
	}()
}

func (pvw *PVWatcher) getTags(pv *v1.PersistentVolume, ctxLogger *zap.Logger) (string, []string) {
	ctxLogger.Debug("Entry getTags()", zap.Reflect("pv", pv))
	volAttributes := pv.Spec.CSI.VolumeAttributes
	// Get user tag list
	tagstr := strings.TrimSpace(volAttributes["tags"])
	var tags []string
	if len(tagstr) > 0 {
		tags = strings.Split(tagstr, ",")
	}
	// append default tags to users tag list
	tags = append(tags, ClusterIDLabel+":"+volAttributes[ClusterIDLabel])
	tags = append(tags, ReclaimPolicyTag+string(pv.Spec.PersistentVolumeReclaimPolicy))
	tags = append(tags, StorageClassTag+pv.Spec.StorageClassName)
	tags = append(tags, NameSpaceTag+pv.Spec.ClaimRef.Namespace)
	tags = append(tags, PVCNameTag+pv.Spec.ClaimRef.Name)
	tags = append(tags, PVNameTag+pv.ObjectMeta.Name)
	tags = append(tags, ProvisionerTag+pvw.provisionerName)
	ctxLogger.Debug("Exit getTags()", zap.String("VolumeCRN", volAttributes[VolumeCRN]), zap.Reflect("tags", tags))
	return volAttributes[VolumeCRN], tags
}

func (pvw *PVWatcher) getVolumeFromPV(pv *v1.PersistentVolume, ctxLogger *zap.Logger) provider.Volume {
	ctxLogger.Debug("Entry getVolume()", zap.Reflect("pv", pv))
	crn, tags := pvw.getTags(pv, ctxLogger)
	volume := provider.Volume{
		VolumeID:   pv.Spec.CSI.VolumeHandle,
		Provider:   provider.VolumeProvider(pvw.config.VPC.VPCBlockProviderType),
		VolumeType: provider.VolumeType(VolumeTypeMap[pv.Spec.CSI.Driver]),
	}
	volume.CRN = crn
	clusterID := pv.Spec.CSI.VolumeAttributes[ClusterIDLabel]
	volume.Attributes = map[string]string{strings.ToLower(ClusterIDLabel): clusterID}
	if pv.Status.Phase == v1.VolumeReleased {
		// Set only status in case of delete operation
		volume.Attributes[VolumeStatus] = VolumeStatusDeleted
	} else {
		volume.Tags = tags
		//Get Capacity and convert to GiB
		capacity := pv.Spec.Capacity[v1.ResourceStorage]
		capacityGiB := BytesToGiB(capacity.Value())
		volume.Capacity = &capacityGiB
		iops := pv.Spec.CSI.VolumeAttributes[IOPSLabel]
		volume.Iops = &iops
		volume.Attributes[VolumeStatus] = VolumeStatusCreated
	}
	ctxLogger.Debug("Exit getVolume()", zap.Reflect("volume", volume))
	return volume
}

func (pvw *PVWatcher) filter(obj interface{}) bool {
	pvw.logger.Debug("Entry filter()", zap.Reflect("obj", obj))
	pv, _ := obj.(*v1.PersistentVolume)
	var provisoinerMatch = false
	if pv != nil && pv.Spec.CSI != nil {
		provisoinerMatch = pv.Spec.CSI.Driver == pvw.provisionerName
	}
	pvw.logger.Debug("Exit filter()", zap.Bool("provisoinerMatch", provisoinerMatch))
	return provisoinerMatch
}

// BytesToGiB converts Bytes to GiB
func BytesToGiB(volumeSizeBytes int64) int {
	return int(volumeSizeBytes / GiB)
}

// GetContextLogger ...
func GetContextLogger(ctx context.Context, isDebug bool) (*zap.Logger, string) {
	return GetContextLoggerWithRequestID(ctx, isDebug, nil)
}

// GetContextLoggerWithRequestID  adds existing requestID in the logger
// The Existing requestID might be coming from ControllerPublishVolume etc
func GetContextLoggerWithRequestID(ctx context.Context, isDebug bool, requestIDIn *string) (*zap.Logger, string) {
	consoleDebugging := zapcore.Lock(os.Stdout)
	consoleErrors := zapcore.Lock(os.Stderr)
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "ts"
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	traceLevel := zap.NewAtomicLevel()
	if isDebug {
		traceLevel.SetLevel(zap.DebugLevel)
	} else {
		traceLevel.SetLevel(zap.InfoLevel)
	}

	core := zapcore.NewTee(
		zapcore.NewCore(zapcore.NewJSONEncoder(encoderConfig), consoleDebugging, zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
			return (lvl >= traceLevel.Level()) && (lvl < zapcore.ErrorLevel)
		})),
		zapcore.NewCore(zapcore.NewJSONEncoder(encoderConfig), consoleErrors, zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
			return lvl >= zapcore.ErrorLevel
		})),
	)
	logger := zap.New(core, zap.AddCaller())
	// generating a unique request ID so that logs can be filter
	if requestIDIn == nil {
		// Generate New RequestID if not provided
		uuid, _ := uid.NewV4() // #nosec G104: Attempt to randomly generate uuid
		requestID := uuid.String()
		requestIDIn = &requestID
	}
	logger = logger.With(zap.String("RequestID", *requestIDIn))
	return logger, *requestIDIn + " "
}
