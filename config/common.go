// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package config

import (
	"io"
	"sync"

	"github.com/mattermost/mattermost-server/model"
	"github.com/pkg/errors"
)

// commonStore enables code sharing between different backing implementations
type commonStore struct {
	emitter

	configLock           sync.RWMutex
	config               *model.Config
	environmentOverrides map[string]interface{}
}

// Get fetches the current, cached configuration.
func (cs *commonStore) Get() *model.Config {
	cs.configLock.RLock()
	defer cs.configLock.RUnlock()

	return cs.config
}

// GetEnvironmentOverrides fetches the configuration fields overridden by environment variables.
func (cs *commonStore) GetEnvironmentOverrides() map[string]interface{} {
	cs.configLock.RLock()
	defer cs.configLock.RUnlock()

	return cs.environmentOverrides
}

// set replaces the current configuration in its entirety, without updating the backing store.
//
// This function assumes no lock has been acquired, as it acquires a write lock itself.
func (cs *commonStore) set(newCfg *model.Config, isValid func(*model.Config) error) (*model.Config, error) {
	cs.configLock.Lock()
	var unlockOnce sync.Once
	defer unlockOnce.Do(cs.configLock.Unlock)

	oldCfg := cs.config

	// TODO: disallow attempting to save a directly modified config (comparing pointers). This
	// wouldn't be an exhaustive check, given the use of pointers throughout the data
	// structure, but might prevent common mistakes. Requires upstream changes first.
	// if newCfg == oldCfg {
	// 	return nil, errors.New("old configuration modified instead of cloning")
	// }

	newCfg = newCfg.Clone()
	newCfg.SetDefaults()

	// Sometimes the config is received with "fake" data in sensitive fielcs. Apply the real
	// data from the existing config as necessary.
	desanitize(oldCfg, newCfg)

	if err := newCfg.IsValid(); err != nil {
		return nil, errors.Wrap(err, "new configuration is invalid")
	}

	// Allow backing-store specific checks.
	if isValid != nil {
		if err := isValid(newCfg); err != nil {
			return nil, err
		}
	}

	// Ideally, Set would persist automatically and abstract this completely away from the
	// client. Doing so requires a few upstream changes first, so for now an explicit Save()
	// remains required.
	// if err := cs.persist(newCfg); err != nil {
	// 	return nil, errors.Wrap(err, "failed to persist")
	// }

	cs.config = newCfg

	unlockOnce.Do(cs.configLock.Unlock)

	// Notify listeners synchronously. Ideally, this would be asynchronous, but existing code
	// assumes this and there would be increased complexity to avoid racing updates.
	cs.invokeConfigListeners(oldCfg, newCfg)

	return oldCfg, nil
}

// load updates the current configuration from the given io.ReadCloser.
//
// This function assumes no lock has been acquired, as it acquires a write lock itself.
func (cs *commonStore) load(f io.ReadCloser, needsSave bool, persist func(*model.Config) error) error {
	allowEnvironmentOverrides := true
	loadedCfg, environmentOverrides, err := unmarshalConfig(f, allowEnvironmentOverrides)
	if err != nil {
		return errors.Wrapf(err, "failed to unmarshal config")
	}

	// SetDefaults generates various keys and salts if not previously configured. Determine if
	// such a change will be made before invoking. This method will not effect the save: that
	// remains the responsibility of the caller.
	needsSave = needsSave || loadedCfg.SqlSettings.AtRestEncryptKey == nil || len(*loadedCfg.SqlSettings.AtRestEncryptKey) == 0
	needsSave = needsSave || loadedCfg.FileSettings.PublicLinkSalt == nil || len(*loadedCfg.FileSettings.PublicLinkSalt) == 0
	needsSave = needsSave || loadedCfg.EmailSettings.InviteSalt == nil || len(*loadedCfg.EmailSettings.InviteSalt) == 0

	loadedCfg.SetDefaults()

	if err := loadedCfg.IsValid(); err != nil {
		return errors.Wrap(err, "invalid config")
	}

	if changed := fixConfig(loadedCfg); changed {
		needsSave = true
	}

	cs.configLock.Lock()
	var unlockOnce sync.Once
	defer unlockOnce.Do(cs.configLock.Unlock)

	if needsSave {
		if err = persist(loadedCfg); err != nil {
			return errors.Wrap(err, "failed to persist required changes after load")
		}
	}

	oldCfg := cs.config
	cs.config = loadedCfg
	cs.environmentOverrides = environmentOverrides

	unlockOnce.Do(cs.configLock.Unlock)

	// Notify listeners synchronously. Ideally, this would be asynchronous, but existing code
	// assumes this and there would be increased complexity to avoid racing updates.
	cs.invokeConfigListeners(oldCfg, loadedCfg)

	return nil
}
