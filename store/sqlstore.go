// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.mau.fi/whatsmeow/util/keys"
)

var ErrInvalidLength = errors.New("database returned byte array with illegal length")

type SQLStore struct {
	*SQLContainer
	JID string

	preKeyLock sync.Mutex
}

var _ IdentityStore = (*SQLStore)(nil)
var _ SessionStore = (*SQLStore)(nil)
var _ PreKeyStore = (*SQLStore)(nil)
var _ SenderKeyStore = (*SQLStore)(nil)

const (
	putIdentityQuery = `
		INSERT INTO whatsmeow_identity_keys (our_jid, their_id, identity) VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, their_id) DO UPDATE SET identity=$3
	`
	getIdentityQuery = `SELECT identity FROM whatsmeow_identity_keys WHERE our_jid=$1 AND their_id=$2`
)

func (s *SQLStore) PutIdentity(address string, key [32]byte) error {
	_, err := s.db.Exec(putIdentityQuery, s.JID, address, key[:])
	return err
}

func (s *SQLStore) IsTrustedIdentity(address string, key [32]byte) (bool, error) {
	var existingIdentity []byte
	err := s.db.QueryRow(getIdentityQuery, s.JID, address).Scan(&existingIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		// Trust if not known, it'll be saved automatically later
		return true, nil
	} else if err != nil {
		return false, err
	} else if len(existingIdentity) != 32 {
		return false, ErrInvalidLength
	}
	return *(*[32]byte)(existingIdentity) == key, nil
}

const (
	getSessionQuery = `SELECT session FROM whatsmeow_sessions WHERE our_jid=$1 AND their_id=$2`
	hasSessionQuery = `SELECT true FROM whatsmeow_sessions WHERE our_jid=$1 AND their_id=$2`
	putSessionQuery = `
		INSERT INTO whatsmeow_sessions (our_jid, their_id, session) VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, their_id) DO UPDATE SET session=$3
	`
)

func (s *SQLStore) GetSession(address string) (session []byte, err error) {
	err = s.db.QueryRow(getSessionQuery, s.JID, address).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (s *SQLStore) HasSession(address string) (has bool, err error) {
	err = s.db.QueryRow(hasSessionQuery, s.JID, address).Scan(&has)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (s *SQLStore) PutSession(address string, session []byte) error {
	_, err := s.db.Exec(putSessionQuery, s.JID, address, session)
	return err
}

const (
	getLastPreKeyIDQuery        = `SELECT MAX(key_id) FROM whatsmeow_pre_keys WHERE jid=$1`
	insertPreKeyQuery           = `INSERT INTO whatsmeow_pre_keys (jid, key_id, key, uploaded) VALUES ($1, $2, $3, $4)`
	getUnuploadedPreKeysQuery   = `SELECT key_id, key FROM whatsmeow_pre_keys WHERE jid=$1 AND uploaded=false ORDER BY key_id LIMIT $2`
	getPreKeyQuery              = `SELECT key_id, key FROM whatsmeow_pre_keys WHERE jid=$1 AND key_id=$2`
	deletePreKeyQuery           = `DELETE FROM whatsmeow_pre_keys WHERE jid=$1 AND key_id=$2`
	markPreKeysAsUploadedQuery  = `UPDATE whatsmeow_pre_keys SET uploaded=true WHERE jid=$1 AND key_id<=$2`
	getUploadedPreKeyCountQuery = `SELECT COUNT(*) FROM whatsmeow_pre_keys WHERE jid=$1 AND uploaded=true`
)

func (s *SQLStore) genOnePreKey(id uint32, markUploaded bool) (*keys.PreKey, error) {
	key := keys.NewPreKey(id)
	_, err := s.db.Exec(insertPreKeyQuery, s.JID, key.KeyID, key.Priv[:], markUploaded)
	return key, err
}

func (s *SQLStore) getNextPreKeyID() (uint32, error) {
	var lastKeyID sql.NullInt32
	err := s.db.QueryRow(getLastPreKeyIDQuery, s.JID).Scan(&lastKeyID)
	if err != nil {
		return 0, fmt.Errorf("failed to query next prekey ID: %w", err)
	}
	return uint32(lastKeyID.Int32) + 1, nil
}

func (s *SQLStore) GenOnePreKey() (*keys.PreKey, error) {
	s.preKeyLock.Lock()
	defer s.preKeyLock.Unlock()
	nextKeyID, err := s.getNextPreKeyID()
	if err != nil {
		return nil, err
	}
	return s.genOnePreKey(nextKeyID, true)
}

func (s *SQLStore) GetOrGenPreKeys(count uint32) ([]*keys.PreKey, error) {
	s.preKeyLock.Lock()
	defer s.preKeyLock.Unlock()

	res, err := s.db.Query(getUnuploadedPreKeysQuery, s.JID, count)
	if err != nil {
		return nil, fmt.Errorf("failed to query existing prekeys: %w", err)
	}
	newKeys := make([]*keys.PreKey, count)
	var existingCount uint32
	for res.Next() {
		var key *keys.PreKey
		key, err = scanPreKey(res)
		if err != nil {
			return nil, err
		} else if key != nil {
			newKeys[existingCount] = key
			existingCount++
		}
	}

	if existingCount < uint32(len(newKeys)) {
		var nextKeyID uint32
		nextKeyID, err = s.getNextPreKeyID()
		if err != nil {
			return nil, err
		}
		for i := existingCount; i < count; i++ {
			newKeys[i], err = s.genOnePreKey(nextKeyID, false)
			nextKeyID++
		}
	}

	return newKeys, nil
}

func scanPreKey(row scannable) (*keys.PreKey, error) {
	var priv []byte
	var id uint32
	err := row.Scan(&id, &priv)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else if len(priv) != 32 {
		return nil, ErrInvalidLength
	}
	return &keys.PreKey{
		KeyPair: *keys.NewKeyPairFromPrivateKey(*(*[32]byte)(priv)),
		KeyID:   id,
	}, nil
}

func (s *SQLStore) GetPreKey(id uint32) (*keys.PreKey, error) {
	return scanPreKey(s.db.QueryRow(getPreKeyQuery, s.JID, id))
}

func (s *SQLStore) RemovePreKey(id uint32) error {
	_, err := s.db.Exec(deletePreKeyQuery, s.JID, id)
	return err
}

func (s *SQLStore) MarkPreKeysAsUploaded(upToID uint32) error {
	_, err := s.db.Exec(markPreKeysAsUploadedQuery, s.JID, upToID)
	return err
}

func (s *SQLStore) UploadedPreKeyCount() (count int, err error) {
	err = s.db.QueryRow(getUploadedPreKeyCountQuery, s.JID).Scan(&count)
	return
}

const (
	getSenderKeyQuery = `SELECT sender_key FROM whatsmeow_sender_keys WHERE our_jid=$1 AND chat_id=$2 AND sender_id=$3`
	putSenderKeyQuery = `
		INSERT INTO whatsmeow_sender_keys (our_jid, chat_id, sender_id, sender_key) VALUES ($1, $2, $3, $4)
		ON CONFLICT (our_jid, chat_id, sender_id) DO UPDATE SET sender_key=$4
	`
)

func (s *SQLStore) PutSenderKey(group, user string, session []byte) error {
	_, err := s.db.Exec(putSenderKeyQuery, s.JID, group, user, session)
	return err
}

func (s *SQLStore) GetSenderKey(group, user string) (key []byte, err error) {
	err = s.db.QueryRow(getSenderKeyQuery, s.JID, group, user).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

const (
	putAppStateSyncKeyQuery = `
		INSERT INTO whatsmeow_app_state_sync_keys (jid, key_id, key_data, timestamp, fingerprint) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (jid, key_id) DO UPDATE SET key_data=$3, timestamp=$4, fingerprint=$5
	`
	getAppStateSyncKeyQuery = `SELECT key_data, timestamp, fingerprint FROM whatsmeow_app_state_sync_keys WHERE jid=$1 AND key_id=$2`
)

func (s *SQLStore) PutAppStateSyncKey(id []byte, key AppStateSyncKey) error {
	_, err := s.db.Exec(putAppStateSyncKeyQuery, s.JID, id, key.Data, key.Timestamp, key.Fingerprint)
	return err
}

func (s *SQLStore) GetAppStateSyncKey(id []byte) (*AppStateSyncKey, error) {
	var key AppStateSyncKey
	err := s.db.QueryRow(getAppStateSyncKeyQuery, s.JID, id).Scan(&key.Data, &key.Timestamp, &key.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return &key, err
}

const (
	putAppStateVersionQuery = `
		INSERT INTO whatsmeow_app_state_version (jid, name, version, hash) VALUES ($1, $2, $3, $4)
		ON CONFLICT (jid, name) DO UPDATE SET version=$3, hash=$4
	`
	getAppStateVersionQuery                 = `SELECT version, hash FROM whatsmeow_app_state_version WHERE jid=$1 AND name=$2`
	deleteAppStateVersionQuery              = `DELETE FROM whatsmeow_app_state_version WHERE jid=$1 AND name=$2`
	putAppStateMutationMACsQuery            = `INSERT INTO whatsmeow_app_state_mutation_macs (jid, name, version, index_mac, value_mac) VALUES `
	deleteAppStateMutationMACsQueryPostgres = `DELETE FROM whatsmeow_app_state_mutation_macs WHERE jid=$1 AND name=$2 AND index_mac=ANY($3)`
	deleteAppStateMutationMACsQueryGeneric  = `DELETE FROM whatsmeow_app_state_mutation_macs WHERE jid=$1 AND name=$2 AND index_mac IN `
	getAppStateMutationMACQuery             = `SELECT value_mac FROM whatsmeow_app_state_mutation_macs WHERE jid=$1 AND name=$2 AND index_mac=$3 ORDER BY version DESC LIMIT 1`
)

func (s *SQLStore) PutAppStateVersion(name string, version uint64, hash [128]byte) error {
	_, err := s.db.Exec(putAppStateVersionQuery, s.JID, name, version, hash[:])
	return err
}

func (s *SQLStore) GetAppStateVersion(name string) (version uint64, hash [128]byte, err error) {
	var uncheckedHash []byte
	err = s.db.QueryRow(getAppStateVersionQuery, s.JID, name).Scan(&version, &uncheckedHash)
	if errors.Is(err, sql.ErrNoRows) {
		// version will be 0 and hash will be an empty array, which is the correct initial state
		err = nil
	} else if err != nil {
		// There's an error, just return it
	} else if len(uncheckedHash) != 128 {
		// This shouldn't happen
		err = ErrInvalidLength
	} else {
		// No errors, convert hash slice to array
		hash = *(*[128]byte)(uncheckedHash)
	}
	return
}

func (s *SQLStore) DeleteAppStateVersion(name string) error {
	_, err := s.db.Exec(deleteAppStateVersionQuery, s.JID, name)
	return err
}

type execable interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func (s *SQLStore) putAppStateMutationMACs(tx execable, name string, version uint64, mutations []AppStateMutationMAC) error {
	values := make([]interface{}, 3+len(mutations)*2)
	queryParts := make([]string, len(mutations))
	values[0] = s.JID
	values[1] = name
	values[2] = version
	for i, mutation := range mutations {
		baseIndex := 3 + i*2
		values[baseIndex] = mutation.IndexMAC
		values[baseIndex+1] = mutation.ValueMAC
		queryParts[i] = fmt.Sprintf("($1, $2, $3, $%d, $%d)", baseIndex+1, baseIndex+2)
	}
	_, err := tx.Exec(putAppStateMutationMACsQuery+strings.Join(queryParts, ","), values...)
	return err
}

const mutationBatchSize = 400

func (s *SQLStore) PutAppStateMutationMACs(name string, version uint64, mutations []AppStateMutationMAC) error {
	if len(mutations) > mutationBatchSize {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("failed to start transaction: %w", err)
		}
		for i := 0; i < len(mutations); i += mutationBatchSize {
			var mutationSlice []AppStateMutationMAC
			if len(mutations) > i+mutationBatchSize {
				mutationSlice, mutations = mutations[:i+mutationBatchSize], mutations[i+mutationBatchSize:]
			} else {
				mutationSlice = mutations
			}
			err = s.putAppStateMutationMACs(tx, name, version, mutationSlice)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		err = tx.Commit()
		if err != nil {
			return fmt.Errorf("failed to commit transaction: %w", err)
		}
		return nil
	} else if len(mutations) > 0 {
		return s.putAppStateMutationMACs(s.db, name, version, mutations)
	}
	return nil
}

func (s *SQLStore) DeleteAppStateMutationMACs(name string, indexMACs [][]byte) (err error) {
	if len(indexMACs) == 0 {
		return
	}
	if s.dialect == "postgres" {
		_, err = s.db.Exec(deleteAppStateMutationMACsQueryPostgres, s.JID, name, indexMACs)
	} else {
		args := make([]interface{}, 2+len(indexMACs))
		args[0] = s.JID
		args[1] = name
		queryParts := make([]string, len(indexMACs))
		for i, item := range indexMACs {
			args[2+i] = item
			queryParts[i] = fmt.Sprintf("$%d", i+3)
		}
		_, err = s.db.Exec(deleteAppStateMutationMACsQueryGeneric+"("+strings.Join(queryParts, ",")+")", args...)
	}
	return
}

func (s *SQLStore) GetAppStateMutationMAC(name string, indexMAC []byte) (valueMAC []byte, err error) {
	err = s.db.QueryRow(getAppStateMutationMACQuery, s.JID, name, indexMAC).Scan(&valueMAC)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}