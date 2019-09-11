package naming_client

import (
	"encoding/json"
	"github.com/nacos-group/nacos-sdk-go/clients/cache"
	"github.com/nacos-group/nacos-sdk-go/model"
	"github.com/nacos-group/nacos-sdk-go/utils"
	nsema "github.com/toolkits/concurrent/semaphore"
	"log"
	"reflect"
	"strings"
	"time"
)

type HostReactor struct {
	serviceInfoMap       cache.ConcurrentMap
	cacheDir             string
	updateThreadNum      int
	serviceProxy         NamingProxy
	pushReceiver         PushReceiver
	subCallback          SubscribeCallback
	updateTimeMap        cache.ConcurrentMap
	updateCacheWhenEmpty bool
}

const DefaultUpdateThreadNum = 20

func NewHostReactor(serviceProxy NamingProxy, cacheDir string, updateThreadNum int, notLoadCacheAtStart bool, subCallback SubscribeCallback, updateCacheWhenEmpty bool) HostReactor {
	if updateThreadNum <= 0 {
		updateThreadNum = DefaultUpdateThreadNum
	}
	hr := HostReactor{
		serviceProxy:         serviceProxy,
		cacheDir:             cacheDir,
		updateThreadNum:      updateThreadNum,
		serviceInfoMap:       cache.NewConcurrentMap(),
		subCallback:          subCallback,
		updateTimeMap:        cache.NewConcurrentMap(),
		updateCacheWhenEmpty: updateCacheWhenEmpty,
	}
	pr := NewPushRecevier(&hr)
	hr.pushReceiver = *pr
	if !notLoadCacheAtStart {
		hr.loadCacheFromDisk()
	}
	go hr.asyncUpdateService()
	return hr
}

func (hr *HostReactor) loadCacheFromDisk() {
	serviceMap := cache.ReadServicesFromFile(hr.cacheDir)
	if serviceMap == nil || len(serviceMap) == 0 {
		return
	}
	for k, v := range serviceMap {
		hr.serviceInfoMap.Set(k, v)
	}
}

func (hr *HostReactor) ProcessServiceJson(result string) {
	service := utils.JsonToService(result)
	if service == nil {
		return
	}
	cacheKey := utils.GetServiceCacheKey(service.Name, service.Clusters)

	oldDomain, ok := hr.serviceInfoMap.Get(cacheKey)
	if ok && !hr.updateCacheWhenEmpty {
		//if instance list is empty,not to update cache
		if len(result) == 0 {
			log.Printf("[ERROR]:do not have useful host, ignore it, name:%s \n", service.Name)
			return
		}
	}
	if !ok || ok && !reflect.DeepEqual(service.Hosts, oldDomain.(model.Service).Hosts) {
		if !ok {
			log.Println("[INFO] service not found in cache " + cacheKey)
		} else {
			log.Printf("[INFO] service key:%s was updated to:%s \n", cacheKey, utils.ToJsonString(service))
		}
		cache.WriteServicesToFile(*service, hr.cacheDir)
		hr.subCallback.ServiceChanged(service)
	}
	hr.updateTimeMap.Set(cacheKey, uint64(utils.CurrentMillis()))
	hr.serviceInfoMap.Set(cacheKey, *service)
}

func (hr *HostReactor) GetServiceInfo(serviceName string, clusters string) model.Service {
	key := utils.GetServiceCacheKey(serviceName, clusters)
	cacheService, ok := hr.serviceInfoMap.Get(key)
	if !ok {
		cacheService = model.Service{Name: serviceName, Clusters: clusters}
		hr.serviceInfoMap.Set(key, cacheService)
		hr.updateServiceNow(serviceName, clusters)
	}
	newService, _ := hr.serviceInfoMap.Get(key)

	return newService.(model.Service)
}

func (hr *HostReactor) GetServiceInfos(serviceNames []string, clusterMap map[string]string, findBack bool) []model.Service {

	serviceInfos := make([]model.Service, 1)

	found := false

	queryWork := func() {
		for i := 0; i < len(serviceNames); i++ {
			serviceName := serviceNames[i]
			clusters, isExist := clusterMap[serviceName]
			if !isExist {
				clusters = ""
			}
			key := utils.GetServiceCacheKey(serviceName, clusters)
			cacheService, ok := hr.serviceInfoMap.Get(key)
			if !ok {
				continue
			}
			found = true
			cacheService = model.Service{Name: serviceName, Clusters: clusters}
			hr.serviceInfoMap.Set(key, cacheService)
			serviceInfos = append(serviceInfos, cacheService.(model.Service))
			if findBack {
				return
			}
		}
	}

	queryWork()

	if !found {
		hr.updateServicesNow(serviceNames, clusterMap, findBack)
		queryWork()
	}

	return serviceInfos
}

func (hr *HostReactor) GetAllServiceInfo(nameSpace string, groupName string, clusters string) []model.Service {
	result, err := hr.serviceProxy.GetAllServiceInfoList(nameSpace, groupName, clusters)
	if err != nil {
		log.Printf("[ERROR]:query all services info return error!nameSpace:%s cluster:%s groupName:%s  err:%s \n", nameSpace, clusters, groupName, err.Error())
		return nil
	}
	if result == "" {
		log.Printf("[ERROR]:query all services info is empty!nameSpace:%s cluster:%s groupName:%s \n", nameSpace, clusters, groupName)
		return nil
	}

	var data []model.Service
	err = json.Unmarshal([]byte(result), &data)
	if err != nil {
		log.Printf("[ERROR]: the result of quering all services info json.Unmarshal error !nameSpace:%s cluster:%s groupName:%s \n", nameSpace, clusters, groupName)
		return nil
	}
	return data
}

func (hr *HostReactor) updateServiceNow(serviceName string, clusters string) {
	result, err := hr.serviceProxy.QueryList(serviceName, clusters, hr.pushReceiver.port, false)
	if err != nil {
		log.Printf("[ERROR]:query list return error!servieName:%s cluster:%s  err:%s \n", serviceName, clusters, err.Error())
		return
	}
	if result == "" {
		log.Printf("[ERROR]:query list is empty!servieName:%s cluster:%s \n", serviceName, clusters)
		return
	}
	hr.ProcessServiceJson(result)
}

func (hr *HostReactor) updateServicesNow(serviceNames []string, clusters map[string]string, findBack bool) {
	names := strings.Join(serviceNames, ",")
	result, err := hr.serviceProxy.QueryListMultiGroup(names, clusters, hr.pushReceiver.port, false, findBack)
	if err != nil {
		log.Printf("[ERROR]:query list return error!servieNames:%s cluster:%s  err:%s \n", names, clusters, err.Error())
		return
	}
	if result == "" {
		log.Printf("[ERROR]:query list is empty!servieNames:%s cluster:%s \n", names, clusters)
		return
	}
	jsonArray := utils.JsonToObject(result, []string{}).([]string)
	for i := 0; i < len(jsonArray); i ++ {
		s := jsonArray[i]
		if strings.Compare("", s) != 0 {
			hr.ProcessServiceJson(s)
		}
	}
}

func (hr *HostReactor) asyncUpdateService() {
	sema := nsema.NewSemaphore(hr.updateThreadNum)
	for {
		for _, v := range hr.serviceInfoMap.Items() {
			service := v.(model.Service)
			lastRefTime, ok := hr.updateTimeMap.Get(utils.GetServiceCacheKey(service.Name, service.Clusters))
			if !ok {
				lastRefTime = uint64(0)
			}
			if uint64(utils.CurrentMillis())-lastRefTime.(uint64) > service.CacheMillis {
				sema.Acquire()
				go func() {
					hr.updateServiceNow(service.Name, service.Clusters)
					sema.Release()
				}()
			}
		}
		time.Sleep(1 * time.Second)
	}

}
