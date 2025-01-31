package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/karimra/gnmic/lockers"
	"github.com/karimra/gnmic/types"
)

const (
	defaultClusterName = "default-cluster"
	retryTimer         = 2 * time.Second
	lockWaitTime       = 100 * time.Millisecond
	apiServiceName     = "gnmic-api"
)

var (
	errNoMoreSuitableServices = errors.New("no more suitable services for this target")
	errNotFound               = errors.New("not found")
)

func (a *App) InitLocker() error {
	if a.Config.Clustering == nil {
		return nil
	}
	if a.Config.Clustering.Locker == nil {
		return errors.New("missing locker config under clustering key")
	}

	if lockerType, ok := a.Config.Clustering.Locker["type"]; ok {
		a.Logger.Printf("starting locker type %q", lockerType)
		if initializer, ok := lockers.Lockers[lockerType.(string)]; ok {
			lock := initializer()
			err := lock.Init(a.ctx, a.Config.Clustering.Locker, lockers.WithLogger(a.Logger))
			if err != nil {
				return err
			}
			a.locker = lock
			return nil
		}
		return fmt.Errorf("unknown locker type %q", lockerType)
	}
	return errors.New("missing locker type field")
}

func (a *App) leaderKey() string {
	return fmt.Sprintf("gnmic/%s/leader", a.Config.Clustering.ClusterName)
}

func (a *App) inCluster() bool {
	if a.Config == nil {
		return false
	}
	return !(a.Config.Clustering == nil)
}

func (a *App) apiServiceRegistration() {
	addr, port, _ := net.SplitHostPort(a.Config.APIServer.Address)
	p, _ := strconv.Atoi(port)

	tags := make([]string, 0, 2+len(a.Config.Clustering.Tags))
	tags = append(tags, fmt.Sprintf("cluster-name=%s", a.Config.Clustering.ClusterName))
	tags = append(tags, fmt.Sprintf("instance-name=%s", a.Config.Clustering.InstanceName))
	tags = append(tags, a.Config.Clustering.Tags...)

	serviceReg := &lockers.ServiceRegistration{
		ID:      a.Config.Clustering.InstanceName + "-api",
		Name:    fmt.Sprintf("%s-%s", a.Config.Clustering.ClusterName, apiServiceName),
		Address: a.Config.Clustering.ServiceAddress,
		Port:    p,
		Tags:    tags,
		TTL:     5 * time.Second,
	}
	if serviceReg.Address == "" {
		serviceReg.Address = addr
	}
	var err error
	a.Logger.Printf("registering service %+v", serviceReg)
	for {
		select {
		case <-a.ctx.Done():
			return
		default:
			err = a.locker.Register(a.ctx, serviceReg)
			if err != nil {
				a.Logger.Printf("api service registration failed: %v", err)
				time.Sleep(retryTimer)
				continue
			}
			return
		}
	}
}

func (a *App) startCluster() {
	if a.locker == nil || a.Config.Clustering == nil {
		return
	}

	// register api service
	go a.apiServiceRegistration()

	leaderKey := a.leaderKey()
	var err error
START:
	for {
		a.isLeader = false
		err = nil
		a.isLeader, err = a.locker.Lock(a.ctx, leaderKey, []byte(a.Config.Clustering.InstanceName))
		if err != nil {
			a.Logger.Printf("failed to acquire leader lock: %v", err)
			time.Sleep(retryTimer)
			continue
		}
		if !a.isLeader {
			time.Sleep(retryTimer)
			continue
		}
		a.isLeader = true
		a.Logger.Printf("%q became the leader", a.Config.Clustering.InstanceName)
		break
	}
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	go a.watchMembers(ctx)
	time.Sleep(a.Config.Clustering.LeaderWaitTimer)
	go a.dispatchTargets(ctx)

	doneCh, errCh := a.locker.KeepLock(a.ctx, leaderKey)
	select {
	case <-doneCh:
		a.Logger.Printf("%q lost leader role", a.Config.Clustering.InstanceName)
		cancel()
		a.isLeader = false
		goto START
	case err := <-errCh:
		a.Logger.Printf("%q failed to maintain the leader key: %v", a.Config.Clustering.InstanceName, err)
		cancel()
		a.isLeader = false
		goto START
	case <-a.ctx.Done():
		return
	}
}

func (a *App) watchMembers(ctx context.Context) {
	serviceName := fmt.Sprintf("%s-%s", a.Config.Clustering.ClusterName, apiServiceName)
START:
	select {
	case <-ctx.Done():
		return
	default:
		membersChan := make(chan []*lockers.Service)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case srvs, ok := <-membersChan:
					if !ok {
						return
					}
					a.updateServices(srvs)
				}
			}
		}()
		err := a.locker.WatchServices(a.ctx, serviceName, []string{"cluster-name=" + a.Config.Clustering.ClusterName}, membersChan, a.Config.Clustering.ServicesWatchTimer)
		if err != nil {
			a.Logger.Printf("failed getting services: %v", err)
			time.Sleep(retryTimer)
			goto START
		}
	}
}

func (a *App) updateServices(srvs []*lockers.Service) {
	a.m.Lock()
	defer a.m.Unlock()

	numNewSrv := len(srvs)
	numCurrentSrv := len(a.apiServices)

	a.Logger.Printf("received service update with %d service(s)", numNewSrv)
	// no new services and no current services, continue
	if numNewSrv == 0 && numCurrentSrv == 0 {
		return
	}

	// no new services and having some services, delete all
	if numNewSrv == 0 && numCurrentSrv != 0 {
		a.Logger.Printf("deleting all services")
		a.apiServices = make(map[string]*lockers.Service)
		return
	}
	// no current services, add all new services
	if numCurrentSrv == 0 {
		for _, s := range srvs {
			a.Logger.Printf("adding service id %q", s.ID)
			a.apiServices[s.ID] = s
		}
		return
	}
	//
	newSrvs := make(map[string]*lockers.Service)
	for _, s := range srvs {
		newSrvs[s.ID] = s
	}
	// delete removed services
	for n := range a.apiServices {
		if _, ok := newSrvs[n]; !ok {
			a.Logger.Printf("deleting service id %q", n)
			delete(a.apiServices, n)
		}
	}
	// add new services
	for n, s := range newSrvs {
		a.Logger.Printf("adding service id %q", n)
		a.apiServices[n] = s
	}
}

func (a *App) dispatchTargets(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			if len(a.apiServices) == 0 {
				a.Logger.Printf("no services found, waiting...")
				time.Sleep(a.Config.Clustering.TargetsWatchTimer)
				continue
			}
			var err error
			a.m.Lock()
			for _, tc := range a.Config.Targets {
				err = a.dispatchTarget(ctx, tc)
				if err != nil {
					a.Logger.Printf("failed to dispatch target %q: %v", tc.Name, err)
				}
				if err == errNotFound {
					// no registered services,
					// no need to continue with other targets,
					// break from the targets loop
					break
				}
				if err == errNoMoreSuitableServices {
					// target has no suitable matching services,
					// continue to next target without wait
					continue
				}
			}
			a.m.Unlock()

			select {
			case <-ctx.Done():
				return
			default:
				time.Sleep(a.Config.Clustering.TargetsWatchTimer)
			}
		}
	}
}

func (a *App) dispatchTarget(ctx context.Context, tc *types.TargetConfig) error {
	locked, err := a.locker.IsLocked(ctx, fmt.Sprintf("gnmic/%s/targets/%s", a.Config.Clustering.ClusterName, tc.Name))
	if err != nil {
		return err
	}
	if a.Config.Debug {
		a.Logger.Printf("target %q is locked: %v", tc.Name, locked)
	}
	if locked {
		return nil
	}
	a.Logger.Printf("dispatching target %q", tc.Name)
	denied := make([]string, 0)
SELECTSERVICE:
	service, err := a.selectService(tc.Tags, denied...)
	if err != nil {
		return err
	}
	if service == nil {
		goto SELECTSERVICE
	}
	a.Logger.Printf("selected service %+v", service)
	// assign target to selected service
	err = a.assignTarget(ctx, tc, service)
	if err != nil {
		// add service to denied list and reselect
		a.Logger.Printf("failed assigning target %q to service %q: %v", tc.Name, service.ID, err)
		denied = append(denied, service.ID)
		goto SELECTSERVICE
	}
	// wait for lock to be acquired
	instanceName := ""
	for _, tag := range service.Tags {
		splitTag := strings.Split(tag, "=")
		if len(splitTag) == 2 && splitTag[0] == "instance-name" {
			instanceName = splitTag[1]
		}
	}
	key := fmt.Sprintf("gnmic/%s/targets/%s", a.Config.Clustering.ClusterName, tc.Name)
	a.Logger.Printf("cluster leader, waiting for lock %q to be acquired by %q", key, instanceName)
	retries := 0
WAIT:
	values, err := a.locker.List(ctx, key)
	if err != nil {
		a.Logger.Printf("failed getting value of %q: %v", key, err)
		time.Sleep(lockWaitTime)
		goto WAIT
	}
	if len(values) == 0 {
		retries++
		if (retries+1)*int(lockWaitTime) >= int(a.Config.Clustering.TargetAssignmentTimeout) {
			a.Logger.Printf("cluster leader, max retries reached for target %q and service %q, reselecting...", tc.Name, service.ID)
			err = a.unassignTarget(tc.Name, service.ID)
			if err != nil {
				a.Logger.Printf("failed to unassign target %q from %q", tc.Name, service.ID)
			}
			goto SELECTSERVICE
		}
		time.Sleep(lockWaitTime)
		goto WAIT
	}
	if instance, ok := values[key]; ok {
		if instance == instanceName {
			a.Logger.Printf("cluster leader, lock %q acquired by %q", key, instanceName)
			return nil
		}
	}
	retries++
	if (retries+1)*int(lockWaitTime) >= int(a.Config.Clustering.TargetAssignmentTimeout) {
		a.Logger.Printf("cluster leader, max retries reached for target %q and service %q, reselecting...", tc.Name, service.ID)
		err = a.unassignTarget(tc.Name, service.ID)
		if err != nil {
			a.Logger.Printf("failed to unassign target %q from %q", tc.Name, service.ID)
		}
		goto SELECTSERVICE
	}
	time.Sleep(lockWaitTime)
	goto WAIT
}

func (a *App) selectService(tags []string, denied ...string) (*lockers.Service, error) {
	numServices := len(a.apiServices)
	switch numServices {
	case 0:
		return nil, errNotFound
	case 1:
		for _, s := range a.apiServices {
			return s, nil
		}
	default:
		// select instance by tags
		matchingInstances := make([]string, 0)
		tagCount := a.getInstancesTagsMatches(tags)
		if len(tagCount) > 0 {
			matchingInstances = a.getHighestTagsMatches(tagCount)
			a.Logger.Printf("current instances with tags=%v: %+v", tags, matchingInstances)
		} else {
			for n := range a.apiServices {
				matchingInstances = append(matchingInstances, strings.TrimSuffix(n, "-api"))
			}
		}
		if len(matchingInstances) == 1 {
			return a.apiServices[matchingInstances[0]+"-api"], nil
		}
		// select instance by load
		load, err := a.getInstancesLoad(matchingInstances...)
		if err != nil {
			return nil, err
		}
		a.Logger.Printf("current instances load: %+v", load)
		// if there are no locks in place, return a random service
		if len(load) == 0 {
			for _, n := range matchingInstances {
				a.Logger.Printf("selected service name: %s", n)
				return a.apiServices[n+"-api"], nil
			}
		}
		for _, d := range denied {
			delete(load, strings.TrimSuffix(d, "-api"))
		}
		a.Logger.Printf("current instances load after filtering: %+v", load)
		// all services were denied
		if len(load) == 0 {
			return nil, errNoMoreSuitableServices
		}
		ss := a.getLowLoadInstance(load)
		a.Logger.Printf("selected service name: %s", ss)
		srv := a.apiServices[ss+"-api"]
		return srv, nil
	}
	return nil, errNotFound
}

func (a *App) getInstancesLoad(instances ...string) (map[string]int, error) {
	// read all current locks held by the cluster
	locks, err := a.locker.List(a.ctx, fmt.Sprintf("gnmic/%s/targets", a.Config.Clustering.ClusterName))
	if err != nil {
		return nil, err
	}
	if a.Config.Debug {
		a.Logger.Println("current locks:", locks)
	}
	load := make(map[string]int)
	// using the read locks, calculate the number of targets each instance has locked
	for _, instance := range locks {
		if _, ok := load[instance]; !ok {
			load[instance] = 0
		}
		load[instance]++
	}
	// for instances that are registered but do not have any lock,
	// add a "0" load
	for _, s := range a.apiServices {
		instance := strings.TrimSuffix(s.ID, "-api")
		if _, ok := load[instance]; !ok {
			load[instance] = 0
		}
	}
	if len(instances) > 0 {
		filteredLoad := make(map[string]int)
		for _, instance := range instances {
			if l, ok := load[instance]; ok {
				filteredLoad[instance] = l
			} else {
				filteredLoad[instance] = 0
			}
		}
		return filteredLoad, nil
	}
	return load, nil
}

// loop through the current cluster load
// find the instance with the lowest load
func (a *App) getLowLoadInstance(load map[string]int) string {
	var ss string
	var low = -1
	for s, l := range load {
		if low < 0 || l < low {
			ss = s
			low = l
		}
	}
	return ss
}

func (a *App) getTargetToInstanceMapping() (map[string]string, error) {
	locks, err := a.locker.List(a.ctx, fmt.Sprintf("gnmic/%s/targets", a.Config.Clustering.ClusterName))
	if err != nil {
		return nil, err
	}
	if a.Config.Debug {
		a.Logger.Println("current locks:", locks)
	}
	for k, v := range locks {
		delete(locks, k)
		locks[filepath.Base(k)] = v
	}
	return locks, nil
}

func (a *App) getInstancesTagsMatches(tags []string) map[string]int {
	maxMatch := make(map[string]int)
	numTags := len(tags)
	if numTags == 0 {
		return maxMatch
	}
	for name, s := range a.apiServices {
		name = strings.TrimSuffix(name, "-api")
		maxMatch[name] = 0
		for i, tag := range s.Tags {
			if i+1 > numTags {
				break
			}
			if tag == tags[i] {
				maxMatch[name]++
				continue
			}
			break
		}
	}
	return maxMatch
}

func (a *App) getHighestTagsMatches(tagsCount map[string]int) []string {
	var ss = make([]string, 0)
	var high = -1
	for s, c := range tagsCount {
		if high < 0 || c > high {
			ss = []string{strings.TrimSuffix(s, "-api")}
			high = c
			continue
		}
		if high == c {
			ss = append(ss, strings.TrimSuffix(s, "-api"))
		}
	}
	return ss
}

func (a *App) deleteTarget(name string) error {
	errs := make([]error, 0, len(a.apiServices))
	for _, s := range a.apiServices {
		url := fmt.Sprintf("http://%s/config/targets/%s", s.Address, name)
		req, err := http.NewRequestWithContext(a.ctx, "DELETE", url, nil)
		if err != nil {
			a.Logger.Printf("failed to create a delete request: %v", err)
			errs = append(errs, err)
			continue
		}

		rsp, err := a.httpClient.Do(req)
		if err != nil {
			a.Logger.Printf("failed deleting target %q: %v", name, err)
			errs = append(errs, err)
			continue
		}
		a.Logger.Printf("received response code=%d, for DELETE %s", rsp.StatusCode, url)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("there was %d error(s) while deleting target %q", len(errs), name)
}

func (a *App) assignTarget(ctx context.Context, tc *types.TargetConfig, service *lockers.Service) error {
	// encode target config
	var err error
	buffer := new(bytes.Buffer)
	err = json.NewEncoder(buffer).Encode(tc)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+service.Address+"/config/targets", buffer)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	a.Logger.Printf("got response code=%d for target %q config add from %q", resp.StatusCode, tc.Name, service.Address)
	if resp.StatusCode > 200 {
		return fmt.Errorf("status code=%d", resp.StatusCode)
	}
	// send target start
	req, err = http.NewRequest(http.MethodPost, "http://"+service.Address+"/targets/"+tc.Name, new(bytes.Buffer))
	if err != nil {
		return err
	}
	resp, err = a.httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	a.Logger.Printf("got response code=%d for target %q assignment from %q", resp.StatusCode, tc.Name, service.Address)
	if resp.StatusCode > 200 {
		return fmt.Errorf("status code=%d", resp.StatusCode)
	}
	return nil
}

func (a *App) unassignTarget(name string, serviceID string) error {
	for _, s := range a.apiServices {
		if s.ID != serviceID {
			continue
		}
		url := fmt.Sprintf("http://%s/targets/%s", s.Address, name)
		ctx, cancel := context.WithTimeout(a.ctx, 500*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
		if err != nil {
			continue
		}
		rsp, err := a.httpClient.Do(req)
		if err != nil {
			continue
		}
		a.Logger.Printf("received response code=%d, for DELETE %s", rsp.StatusCode, url)
		break
	}
	return nil
}
