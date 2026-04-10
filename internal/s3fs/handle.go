// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package s3fs

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketInodes = []byte("inodes") // inode -> S3 key
	bucketKeys   = []byte("keys")   // S3 key -> inode
	bucketMeta   = []byte("meta")   // metadata (e.g. next inode counter)
	keyNextInode = []byte("next_inode")
)

const rootInode uint64 = 1

// HandleStore manages bidirectional mapping between S3 keys, inodes, and NFS handles.
// NFS handles are 8-byte big-endian encoded inode numbers.
type HandleStore struct {
	db        *bolt.DB
	mu        sync.RWMutex
	nextInode uint64

	// In-memory cache for fast lookups
	inodeToKey map[uint64]string
	keyToInode map[string]uint64
}

// NewHandleStore opens or creates a bbolt database for handle management.
func NewHandleStore(dbPath string) (*HandleStore, error) {
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("open bbolt: %w", err)
	}

	store := &HandleStore{
		db:         db,
		inodeToKey: make(map[uint64]string),
		keyToInode: make(map[string]uint64),
	}

	if err := store.init(); err != nil {
		db.Close()
		return nil, err
	}

	return store, nil
}

func (s *HandleStore) init() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{bucketInodes, bucketKeys, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("create bucket %s: %w", bucket, err)
			}
		}

		// Load or initialize the inode counter
		meta := tx.Bucket(bucketMeta)
		if v := meta.Get(keyNextInode); v != nil {
			s.nextInode = binary.BigEndian.Uint64(v)
		} else {
			s.nextInode = rootInode + 1
		}

		// Ensure root inode exists.
		// Use a sentinel key "\x00" for the root directory because bbolt
		// rejects zero-length keys. The in-memory maps still use "" for
		// the root so the rest of the code is unaffected.
		inodes := tx.Bucket(bucketInodes)
		keys := tx.Bucket(bucketKeys)
		rootKey := []byte("\x00")

		if inodes.Get(uint64ToBytes(rootInode)) == nil {
			if err := inodes.Put(uint64ToBytes(rootInode), rootKey); err != nil {
				return err
			}
			if err := keys.Put(rootKey, uint64ToBytes(rootInode)); err != nil {
				return err
			}
		}

		// Load all mappings into memory.
		return inodes.ForEach(func(k, v []byte) error {
			inode := binary.BigEndian.Uint64(k)
			key := boltToS3Key(v)
			s.inodeToKey[inode] = key
			s.keyToInode[key] = inode
			return nil
		})
	})
}

// GetOrCreateInode returns the inode for an S3 key, creating one if it doesn't exist.
func (s *HandleStore) GetOrCreateInode(s3Key string) (uint64, error) {
	s.mu.RLock()
	if inode, ok := s.keyToInode[s3Key]; ok {
		s.mu.RUnlock()
		return inode, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check after acquiring write lock
	if inode, ok := s.keyToInode[s3Key]; ok {
		return inode, nil
	}

	if s.nextInode == 0 { // wrapped around
		return 0, fmt.Errorf("inode counter overflow: maximum inodes reached")
	}

	inode := s.nextInode
	s.nextInode++

	err := s.db.Update(func(tx *bolt.Tx) error {
		inodes := tx.Bucket(bucketInodes)
		keys := tx.Bucket(bucketKeys)
		meta := tx.Bucket(bucketMeta)

		if err := inodes.Put(uint64ToBytes(inode), s3KeyToBolt(s3Key)); err != nil {
			return err
		}
		if err := keys.Put(s3KeyToBolt(s3Key), uint64ToBytes(inode)); err != nil {
			return err
		}
		return meta.Put(keyNextInode, uint64ToBytes(s.nextInode))
	})
	if err != nil {
		return 0, fmt.Errorf("persist inode: %w", err)
	}

	s.inodeToKey[inode] = s3Key
	s.keyToInode[s3Key] = inode

	return inode, nil
}

// GetInode returns the inode for an S3 key, or 0 if not found.
func (s *HandleStore) GetInode(s3Key string) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keyToInode[s3Key]
}

// GetKey returns the S3 key for an inode, or empty string if not found.
func (s *HandleStore) GetKey(inode uint64) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.inodeToKey[inode]
	return key, ok
}

// RemoveByKey removes the inode mapping for an S3 key.
func (s *HandleStore) RemoveByKey(s3Key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	inode, ok := s.keyToInode[s3Key]
	if !ok {
		return nil
	}

	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketInodes).Delete(uint64ToBytes(inode)); err != nil {
			return err
		}
		return tx.Bucket(bucketKeys).Delete(s3KeyToBolt(s3Key))
	})
	if err != nil {
		return fmt.Errorf("remove inode: %w", err)
	}

	delete(s.inodeToKey, inode)
	delete(s.keyToInode, s3Key)
	return nil
}

// RenameKey updates the S3 key for an existing inode (used by Rename).
func (s *HandleStore) RenameKey(oldKey, newKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	inode, ok := s.keyToInode[oldKey]
	if !ok {
		return os.ErrNotExist
	}

	err := s.db.Update(func(tx *bolt.Tx) error {
		inodes := tx.Bucket(bucketInodes)
		keys := tx.Bucket(bucketKeys)

		if err := keys.Delete(s3KeyToBolt(oldKey)); err != nil {
			return err
		}
		if err := keys.Put(s3KeyToBolt(newKey), uint64ToBytes(inode)); err != nil {
			return err
		}
		return inodes.Put(uint64ToBytes(inode), s3KeyToBolt(newKey))
	})
	if err != nil {
		return fmt.Errorf("rename inode: %w", err)
	}

	delete(s.keyToInode, oldKey)
	s.keyToInode[newKey] = inode
	s.inodeToKey[inode] = newKey
	return nil
}

// InodeToHandle converts an inode number to an 8-byte NFS handle.
func InodeToHandle(inode uint64) []byte {
	return uint64ToBytes(inode)
}

// HandleToInode converts an 8-byte NFS handle back to an inode number.
func HandleToInode(handle []byte) (uint64, error) {
	if len(handle) < 8 {
		return 0, fmt.Errorf("invalid handle length: %d", len(handle))
	}
	return binary.BigEndian.Uint64(handle[:8]), nil
}

// RootHandle returns the 8-byte NFS handle for the root directory.
func RootHandle() []byte {
	return uint64ToBytes(rootInode)
}

// Close closes the underlying bbolt database.
func (s *HandleStore) Close() error {
	return s.db.Close()
}

func uint64ToBytes(v uint64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, v)
	return buf
}

// s3KeyToBolt encodes an S3 key for storage in bbolt. Empty keys (the root
// directory) are stored as the sentinel "\x00" because bbolt rejects
// zero-length keys.
func s3KeyToBolt(s3Key string) []byte {
	if s3Key == "" {
		return []byte("\x00")
	}
	return []byte(s3Key)
}

// boltToS3Key decodes a bbolt key back to an S3 key.
func boltToS3Key(b []byte) string {
	if len(b) == 1 && b[0] == 0 {
		return ""
	}
	return string(b)
}
