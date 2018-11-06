package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/rancher/longhorn-manager/datastore"
	"github.com/rancher/longhorn-manager/types"

	longhorn "github.com/rancher/longhorn-manager/k8s/pkg/apis/longhorn/v1alpha1"
	lhinformers "github.com/rancher/longhorn-manager/k8s/pkg/client/informers/externalversions/longhorn/v1alpha1"
)

const (
	VersionTagLatest = "latest"
)

var (
	ownerKindSetting = longhorn.SchemeGroupVersion.WithKind("Setting").String()

	upgradeCheckInterval          = time.Duration(24) * time.Hour
	settingControllerResyncPeriod = time.Hour
	checkUpgradeURL               = "http://upgrade-responder.longhorn.rancher.io/v1/checkupgrade"
)

type SettingController struct {
	kubeClient    clientset.Interface
	eventRecorder record.EventRecorder

	ds *datastore.DataStore

	sStoreSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface

	lastUpgradeCheckedTimestamp time.Time
	version                     string
}

type Version struct {
	Name        string // must be in semantic versioning
	ReleaseDate string
	Tags        []string
}

type CheckUpgradeRequest struct {
	LonghornVersion string `json:"longhornVersion"`
}

type CheckUpgradeResponse struct {
	Versions []Version `json:"versions"`
}

func NewSettingController(
	ds *datastore.DataStore,
	scheme *runtime.Scheme,
	settingInformer lhinformers.SettingInformer,
	kubeClient clientset.Interface, version string) *SettingController {

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	// TODO: remove the wrapper when every clients have moved to use the clientset.
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.CoreV1().RESTClient()).Events("")})

	sc := &SettingController{
		kubeClient:    kubeClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme, v1.EventSource{Component: "longhorn-setting-controller"}),

		ds: ds,

		sStoreSynced: settingInformer.Informer().HasSynced,

		queue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "longhorn-setting"),
		version: version,
	}

	settingInformer.Informer().AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			setting := obj.(*longhorn.Setting)
			sc.enqueueSetting(setting)
		},
		UpdateFunc: func(old, cur interface{}) {
			curSetting := cur.(*longhorn.Setting)
			sc.enqueueSetting(curSetting)
		},
		DeleteFunc: func(obj interface{}) {
			setting := obj.(*longhorn.Setting)
			sc.enqueueSetting(setting)
		},
	}, settingControllerResyncPeriod)

	return sc
}

func (sc *SettingController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer sc.queue.ShutDown()

	logrus.Infof("Start Longhorn Setting controller")
	defer logrus.Infof("Shutting down Longhorn Setting controller")

	if !controller.WaitForCacheSync("longhorn settings", stopCh, sc.sStoreSynced) {
		return
	}

	go wait.Until(sc.worker, time.Second, stopCh)

	<-stopCh
}

func (sc *SettingController) worker() {
	for sc.processNextWorkItem() {
	}
}

func (sc *SettingController) processNextWorkItem() bool {
	key, quit := sc.queue.Get()

	if quit {
		return false
	}
	defer sc.queue.Done(key)

	err := sc.syncSetting(key.(string))
	sc.handleErr(err, key)

	return true
}

func (sc *SettingController) handleErr(err error, key interface{}) {
	if err == nil {
		sc.queue.Forget(key)
		return
	}

	if sc.queue.NumRequeues(key) < maxRetries {
		logrus.Warnf("Error syncing Longhorn setting %v: %v", key, err)
		sc.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	logrus.Warnf("Dropping Longhorn setting %v out of the queue: %v", key, err)
	sc.queue.Forget(key)
}

func (sc *SettingController) syncSetting(key string) (err error) {
	defer func() {
		err = errors.Wrapf(err, "fail to sync setting for %v", key)
	}()

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	// We only process upgrade checker for now
	if name != string(types.SettingNameUpgradeChecker) {
		return nil
	}

	upgradeCheckerEnabled, err := sc.ds.GetSettingAsBool(types.SettingNameUpgradeChecker)
	if err != nil {
		return err
	}

	latestLonghornVersion, err := sc.ds.GetSetting(types.SettingNameLatestLonghornVersion)
	if err != nil {
		return err
	}

	if upgradeCheckerEnabled == false {
		if latestLonghornVersion.Value != "" {
			latestLonghornVersion.Value = ""
			if _, err := sc.ds.UpdateSetting(latestLonghornVersion); err != nil {
				return err
			}
		}
		// reset timestamp so it can be triggered immediately when
		// setting changes next time
		sc.lastUpgradeCheckedTimestamp = time.Time{}
		return nil
	}

	now := time.Now()
	if now.Before(sc.lastUpgradeCheckedTimestamp.Add(upgradeCheckInterval)) {
		return nil
	}

	oldVersion := latestLonghornVersion.Value
	latestLonghornVersion.Value, err = sc.CheckLatestLonghornVersion()
	if err != nil {
		return err
	}

	sc.lastUpgradeCheckedTimestamp = now

	if latestLonghornVersion.Value != oldVersion {
		logrus.Infof("New Longhorn version %v is available", latestLonghornVersion.Value)
		if _, err := sc.ds.UpdateSetting(latestLonghornVersion); err != nil {
			return err
		}
	}
	return nil
}

func (sc *SettingController) CheckLatestLonghornVersion() (string, error) {
	var (
		resp    CheckUpgradeResponse
		content bytes.Buffer
	)
	req := &CheckUpgradeRequest{
		LonghornVersion: sc.version,
	}
	if err := json.NewEncoder(&content).Encode(req); err != nil {
		return "", err
	}
	r, err := http.Post(checkUpgradeURL, "application/json", &content)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		return "", err
	}

	latestVersion := ""
	for _, v := range resp.Versions {
		found := false
		for _, tag := range v.Tags {
			if tag == VersionTagLatest {
				found = true
				break
			}
		}
		if found {
			latestVersion = v.Name
			break
		}
	}
	if latestVersion == "" {
		return "", fmt.Errorf("cannot find latest version in response: %+v", resp)
	}

	return latestVersion, nil
}

func (sc *SettingController) enqueueSetting(setting *longhorn.Setting) {
	key, err := controller.KeyFunc(setting)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", setting, err))
		return
	}

	sc.queue.AddRateLimited(key)
}
