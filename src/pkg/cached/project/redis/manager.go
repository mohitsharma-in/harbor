// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package redis

import (
	"context"
	"time"

	"github.com/goharbor/harbor/src/common/utils"
	libcache "github.com/goharbor/harbor/src/lib/cache"
	"github.com/goharbor/harbor/src/lib/config"
	"github.com/goharbor/harbor/src/lib/errors"
	"github.com/goharbor/harbor/src/lib/log"
	"github.com/goharbor/harbor/src/lib/q"
	"github.com/goharbor/harbor/src/lib/retry"
	"github.com/goharbor/harbor/src/pkg/cached"
	"github.com/goharbor/harbor/src/pkg/project"
	"github.com/goharbor/harbor/src/pkg/project/models"
)

var _ CachedManager = &Manager{}

// CachedManager is the interface combines raw resource Manager and cached Manager for better extension.
type CachedManager interface {
	// Manager is the raw resource Manager.
	project.Manager
	// Manager is the common interface for resource cache.
	cached.Manager
}

// Manager is the cached Manager implemented by redis.
type Manager struct {
	// delegator delegates the raw crud to DAO.
	delegator project.Manager
	// client returns the redis cache client.
	client func() libcache.Cache
	// keyBuilder builds cache object key.
	keyBuilder *cached.ObjectKey
	// lifetime is the cache life time.
	lifetime time.Duration
}

// NewManager returns the redis cache Manager.
func NewManager(m project.Manager) *Manager {
	return &Manager{
		delegator:  m,
		client:     func() libcache.Cache { return libcache.Default() },
		keyBuilder: cached.NewObjectKey(cached.ResourceTypeProject),
		lifetime:   time.Duration(config.CacheExpireHours()) * time.Hour,
	}
}

func (m *Manager) Create(ctx context.Context, project *models.Project) (int64, error) {
	return m.delegator.Create(ctx, project)
}

func (m *Manager) Count(ctx context.Context, query *q.Query) (total int64, err error) {
	return m.delegator.Count(ctx, query)
}

func (m *Manager) List(ctx context.Context, query *q.Query) ([]*models.Project, error) {
	return m.delegator.List(ctx, query)
}

func (m *Manager) ListRoles(ctx context.Context, projectID int64, userID int, groupIDs ...int) ([]int, error) {
	return m.delegator.ListRoles(ctx, projectID, userID, groupIDs...)
}

func (m *Manager) Delete(ctx context.Context, id int64) error {
	p, err := m.Get(ctx, id)
	if err != nil {
		return err
	}

	// pass on delete operation
	if err := m.delegator.Delete(ctx, id); err != nil {
		return err
	}
	// clean cache
	m.cleanUp(ctx, p)
	return nil
}

func (m *Manager) Get(ctx context.Context, idOrName interface{}) (*models.Project, error) {
	var (
		key string
		err error
	)

	id, name, err := utils.ParseProjectIDOrName(idOrName)
	if err != nil {
		return nil, err
	}

	if id != 0 {
		key, err = m.keyBuilder.Format("id", id)
		if err != nil {
			return nil, err
		}
	}

	if name != "" {
		key, err = m.keyBuilder.Format("name", name)
		if err != nil {
			return nil, err
		}
	}

	p := &models.Project{}
	if err = m.client().Fetch(ctx, key, p); err == nil {
		return p, nil
	}

	log.Debugf("get project %v from cache error: %v, will query from database.", idOrName, err)

	p, err = m.delegator.Get(ctx, idOrName)
	if err != nil {
		return nil, err
	}

	if err = m.client().Save(ctx, key, p, m.lifetime); err != nil {
		// log error if save to cache failed
		log.Debugf("save project %s to cache error: %v", p.Name, err)
	}

	return p, nil
}

// cleanUp cleans up data in cache.
func (m *Manager) cleanUp(ctx context.Context, p *models.Project) {
	// clean index by id
	idIdx, err := m.keyBuilder.Format("id", p.ProjectID)
	if err != nil {
		log.Errorf("format project id key error: %v", err)
	} else {
		// retry to avoid dirty data
		if err = retry.Retry(func() error { return m.client().Delete(ctx, idIdx) }); err != nil {
			log.Errorf("delete project cache key %s error: %v", idIdx, err)
		}
	}

	// clean index by name
	nameIdx, err := m.keyBuilder.Format("name", p.Name)
	if err != nil {
		log.Errorf("format project name key error: %v", err)
	} else {
		if err = retry.Retry(func() error { return m.client().Delete(ctx, nameIdx) }); err != nil {
			log.Errorf("delete project cache key %s error: %v", nameIdx, err)
		}
	}
}

func (m *Manager) ResourceType(ctx context.Context) string {
	return cached.ResourceTypeProject
}

func (m *Manager) CountCache(ctx context.Context) (int64, error) {
	// prefix is resource type
	keys, err := m.client().Keys(ctx, m.ResourceType(ctx))
	if err != nil {
		return 0, err
	}

	return int64(len(keys)), nil
}

func (m *Manager) DeleteCache(ctx context.Context, key string) error {
	return m.client().Delete(ctx, key)
}

func (m *Manager) FlushAll(ctx context.Context) error {
	// prefix is resource type
	keys, err := m.client().Keys(ctx, m.ResourceType(ctx))
	if err != nil {
		return err
	}

	var errs errors.Errors
	for _, key := range keys {
		if err = m.client().Delete(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}

	if errs.Len() > 0 {
		return errs
	}

	return nil
}
