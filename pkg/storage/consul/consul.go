package consul

import {
	"errors"
	//"time"
	"reflect"
	"strings"
  
	"k8s.io/kubernetes/pkg/conversion"
	"k8s.io/kbuernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/storage"
	"k8s.io/kubernetes/pkg/util"
	//"k8s.io/kubernetes/pkg/watch"
  
	consulapi "github.com/hashicorp/consul/api"
	"golang.org/x/net/context"
}

type ConsulKvStorageConfig struct {
	Config      ConsulConfig
	Codec       runtime.Codec
}

// implements storage.Config
func (c *ConsulKvStorageConfig) GetType() string {
	return "consulkv"
}

// implements storage.Config
func (c *ConsulKvStorageConfig) NewStorage() (storage.Interface, error) {
	return newConsulKvStorage( c.Config, c.Codec )
}

type ConsulConfig struct {
	// TODO add specific configuration values for k8s to pass to consul client
}

func (c *ConsulConfig)  getConsulApiConfig() consulapi.Config {
	config := consulapi.DefaultConfig()
	  
	// TODO do stuff to propagate configuration values from our structure
	// to theirs
	  
	return config
}


func newConsulKvStorage(config *ConsulConfig, codec runtime.Codec) (ConsulKvStorage, error) {
	client, err := consulapi.NewClient(config.getConsulApiConfig())
	if err != nil {
		return nil, err
	}
	return ConsulKvStorage {
		ConsulKv:   client.KV(),
		codec:      codec,
		versioner:  nil, // TODO
		copier:     api.Scheme,
	}
}

type ConsulKvStorage struct {
	ConsulKv    consulapi.KV
	codec       runtime.Codec
	copier      runtime.ObjectCopier
	versioner   storage.Versioner
	pathPrefix  string
}

func (s *ConsulKvStorage) Codec() runtime.Codec {
	return s.codec
}

func (s *ConsulKvStorage) Backends(ctx context.Context) []string {
	// TODO
	return []string{}
}

func (s *ConsulKvStorage) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	trace := util.NewTrace("ConsulKvStorage::Create " + getTypeName(obj))
	defer trace.LogIfLong(250 * time.Millisecond)
	if ctx == nil {
		glog.Errorf("Context is nil")
	}
	key = s.prefixKey(key)
	data, err := runtime.Encode(s.codec, obj)
	trace.Step("Object encoded")
	if err != nil {
		return err
	}
	if version, err := s.versioner.ObjectResourceVersion(obj); err == nil && version != 0 {
		return errors.New("resourceVersion may not be set on objects to be created")
	}
	trace.Step("Version checked")
	  
	// TODO: metrics and stuff
	// startTime := time.Now()
	kv := &consulapi.KVPair{
		Key:            key,
		Value:          data,
		ModifyIndex:    0,    // explicitly set to indicate Create-Only behavior
		// TODO: TTL, if and when this functionality becomes available
	}
	succeeded, _, err := s.ConsulKv.CAS_v2( &kv, nil )
	// metrics.RecordStuff
	trace.Step("Object created")
	if err != nil {
		return toStorageErr( err, key, 0 )
	}
	if out != nil {
		if _, err := conversion.EnforcePtr(out); err != nil {
			panic("unable to convert output object to pointer")
		}
		err = s.extractObj(&kv, err, out, false)
	}
	return err
}

func (s *ConsulKvStorage) Set(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	if ctx == nil {
		glog.Errorf("Context is nil")
	}

	version := uint64(0)
	var err error
	if version, err = s.versioner.ObjectResourceVersion(obj); err != nil {
		return errors.New("couldn't get resourceVersion from object")
	}
	if version != 0 {
		// We cannot store object with resourceVersion in etcd, we need to clear it here.
		if err := h.versioner.UpdateObject(obj, nil, 0); err != nil {
			return errors.New("resourceVersion cannot be set on objects store in etcd")
		}
	}
	
	data, err := runtime.Encode(s.codec, obj)
	if err != nil {
		return err
	}
	key = s.prefixEtcdKey(key)

	kv := consulapi.KVPair{
		Key:         key,
		ModifyIndex: version,
		Value:       data,
	}
	
	// Create and CAS are the same operation distinguished by
	// the same distinguishing value here - ModifyIndex == 0
	success, err := s.ConsulKv.CAS_v2(&kv, nil)
	
	if out != nil {
		if _, err := conversion.EnforcePtr(out); err != nil {
			panic("unable to convert output object to pointer")
		}
		err := s.extractObj(&kv, err, out, false)
	}
	return err
}

func (s *ConsulKvStorage) Delete(ctx context.Context, key string, out runtime.Object, preconditions *storage.Preconditions) error {
	if ctx == nil {
		glog.Errorf("Context is nil")
	}
	key = s.prefixKey(key)
	v, err := conversion.EnforcePtr(out)
	if err != nil {
		panic("unable to convert output object to pointer")
	}
	if preconditions != nil {
		obj := reflect.New(v.Type()).Interface().(runtime.Object)
	}
	// kv declared outside of the spin-loop so that we can decode subsequent successful Gets
	// in the event that another client deletes our key before we do.. this value is possibly
	// lacking certified freshness
	var kv consulapi.KVPair
	// spin cycle Get;DeleteCAS to ensure the returned value is the exact value prior to deletion
	// TODO: perhaps a timeout or spincount would be wise here
	for {
		// empty QueryOptions is explicitly setting AllowStale to false
		kv, _, err := s.ConsulKv.Get( key, &consulapi.QueryOptions{} )
		if err != nil {
			if isErrNotFound( err ) {
				// if we have previously succeeded in getting a value, but not deleting it
				// then decode the most recently gotten value (unless we have already done
				// so in order to test for preconditions)
				if len(kv.Value) != 0 && preconditions == nil {
					err = s.extractObj(&kv, err, out, false)
				}
			}
			return toStorageErr(err, key, 0)
		}
		if preconditions != nil {
			err = s.extractObj(&kv, err, out, false)
			if err != nil {
				return toStorageErr(err, key, 0)
			}
			if err := checkPreconditions(preconditions, obj); err != nil {
				return toStorageErr(err, key, 0)
			}
		}
		succeeded, _, err := s.ConsulKv.DeleteCAS(&kv,nil)
		if err != nil {
			if isErrNotFound( err ) {
				// if we have previously succeeded in getting a value, but not deleting it
				// then decode the most recently gotten value (unless we have already done
				// so in order to test for preconditions)
				if len(kv.Value) != 0 && preconditions == nil {
					err = s.extractObj(&kv, err, out, false)
				}
			}
			return toStorageErr(err, key, 0)
		}
		if !succeeded {
			glog.Infof("delection of %s failed because of a conflict, going to retry", key)
		} else {
			return toStorageErr(err, key, 0)
		}
	}
}

func (s *ConsulKvStorage) Watch(ctx context.Context, key string, resourceVersion string, filter storage.FilterFunc) (watch.Interface, error) {
	
}

func (s *ConsulKvStorage) WatchList(ctx context.Context, key string, resourceVersion string, filter storage.FilterFunc) (watch.Interface, error) {
	
}

func (s *ConsulKvStorage) Get(ctx context.Context, key string, objPtr runtime.Object, ignoreNotFound bool) error {
	if ctx == nil {
		glog.Errorf("Context is nil")
	}
	key = s.prefixEtcdKey(key)
	kv, _, err := s.ConsulKv.Get(key, &consulapi.QueryOptions{})
	if err != nil {
		return toStorageErr(err, key, 0)
	}
	err = s.extractObj(&kv, err, out, false)
	return toStorageErr(err, key, 0)
}

type keyFilterFunc func(key string) bool

func (s *ConsulKvStorage) GetToList(ctx context.Context, key string, filter storage.FilterFunc, listObj runtime.Object) error {
	if ctx == nil {
		glog.Errorf("Context is nil")
	}
	key = s.prefixKey(key)
	// ensure that our path is terminated with a / to make it a directory
	if !HasSufix( key, "/" ) {
		key = key + "/"
	}
	
	// create a filter that will omit deep finds
	myLastIndex = strings.LastIndex(key, "/")
	fnKeyFilter := func(key string) bool {
		return myLastIndex == strings.LastIndex(key, "/")
	}
	
	return listInternal("GetToList ", key, fnKeyFilter, filter, listObj)
}

func (s *ConsulKvStorage) List(ctx context.Context, key string, resourceVersion string, filter storage.FilterFunc, listObj runtime.Object) error {
	if ctx == nil {
		glog.Errorf("Context is nil")
	}
	key = s.prefixKey(key)
	return listInternal("List ", key, func(string) bool {return true}, filter, listObj)
}

func (s *ConsulKvStorage) listInternal(fnName string, key string, keyFilter keyFilterFunc, filter storage.FilterFunc, listObj runtime.Object) error { 
	trace := util.NewTrace(fnName + getTypeName(listObj))
	listPtr, err := meta.GetItemsPtr(listObj)
	if err != nil {
		return err
	}
	v, err := conversion.EnforcePtr(listPtr)
	if err != nil || v.Kind() != reflect.Slice {
		// This should not happen at runtime.
		panic("need ptr to slice")
	}
	startTime := time.Now()
	trace.Step("About to read consul kv list")

	kvlist, _, err := s.ConsulKv.List(key, nil);
	
	// TODO: record metrics
	if err != nil {
		return toStorageErr(err, key, 0)
	}
	
	// unlike etcd, reads are not rafted, so they don't get an index of their own
	// so in order to version the resulting list consistantly, we apply the index
	// of the most recent member 
	maxIndex := uint64(0)
	
	for _, kv := range kvlist {
		if keyFilter(kv.Key) {
			obj, _, err := s.codec.Decode(kv.Value, nil, reflect.New(v.Type().Elem()).Interface().(runtime.Object))
			if err != nil {
				return err
			}
		
			// being unable to set the version does not prevent the object from being extracted
			_ = s.versioner.UpdateObject(obj, nil, kv.ModifyIndex)
			if filter(obj) {
				v.Set(reflect.Append(v, reflect.ValueOf(obj).Elem()))
			}
			// TODO: contemplate all the possible meanings of the word 'cache'
			//if kv.ModifyIndex != 0 {
			//	s.addToCache(kv.ModifyIndex, obj)
			//}
			if maxIndex < kv.ModifyIndex {
				maxIndex = kv.ModifyIndex
			}
		}
	}
	if err := s.versioner.UpdateList(listObj, maxIndex); err != nil {
		return err
	}
	return nil
}

func (s *ConsulKvStorage) GuaranteedUpdate(ctx context.Context, key string, ptrToType runtime.Object, ignoreNotFound bool, preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc) error {
	
}

func (s *ConsulKvStorage) extractObj(kv *consulapi.KVPair, inErr error, objPtr runtime.Object, ignoreNotFound bool) error {
	if inErr != nil || len(kv.Value) == 0 {
		if ignoreNotFound {
			v, err := conversion.EnforcePtr(objPtr)
			if err != nil {
				return err
			}
			v.Set(reflect.Zero(v.Type()))
			return nil
		} else if inErr != nil {
			return inErr
		}
		return fmt.Errorf("unable to locate value in response for key: %#v", kv)
	}
	out, gvk, err := s.codec.Decode(kv.Value, nil, objPtr)
	if err != nil {
		return err
	}
	if out != objPtr {
		return fmt.Errorf("unable to decode object %s into %v", gvk.String(), reflect.TypeOf(objPtr))
	}
	_ = s.versioner.UpdateObject(objPtr, kv.ModifyIndex)
	return err
}

func checkPreconditions(preconditions *storage.Preconditions, out runtime.Object) error {
	if preconditions == nil {
		return nil
	}
	objMeta, err := api.ObjectMetaFor(out)
	if err != nil {
		return storage.NewInternalErrorf("can't enforce preconditions %v on un-introspectable object %v, got error: %v", *preconditions, out, err)
	}
	if preconditions.UID != nil && *preconditions.UID != objMeta.UID {
		// TODO: replace with non-etcd error coding
		return etcd.Error{Code: etcd.ErrorCodeTestFailed, Message: fmt.Sprintf("the UID in the precondition (%s) does not match the UID in record (%s). The object might have been deleted and then recreated", *preconditions.UID, objMeta.UID)}
	}
	return nil
}

func (s *ConsulKvStorage) prefixKey(key string) string {
	if strings.HasPrefix(key, s.pathPrefix) {
		return key
	}
	return path.Join(s.pathPrefix, key)
}

func toStorageErr(err error) error {
	// TODO: Translate errors into values consistent with k8s
	return err
}