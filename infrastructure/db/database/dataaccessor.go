package database

// DataAccessor defines the common interface by which data gets
// accessed in a generic kaspad database.
type DataAccessor interface {
	// Put sets the value for the given key. It overwrites
	// any previous value for that key.
	Put(key *Key, value []byte) error

	// Get gets the value for the given key. It returns
	// ErrNotFound if the given key does not exist.
	Get(key *Key) ([]byte, error)

	// Has returns true if the database does contains the
	// given key.
	Has(key *Key) (bool, error)

	// Delete deletes the value for the given key. Will not
	// return an error if the key doesn't exist.
	Delete(key *Key) error

	// AppendToStore appends the given data to the store
	// defined by storeName. This function returns a location
	// handle that's meant to be stored and later used
	// when querying the data that has just now been inserted.
	AppendToStore(storeName string, data []byte) (StoreLocation, error)

	// RetrieveFromStore retrieves data from the store defined by
	// storeName using the given location handle. It returns
	// ErrNotFound if the location does not exist. See
	// AppendToStore for further details.
	RetrieveFromStore(storeName string, location StoreLocation) ([]byte, error)

	// DeleteFromStoreUpToLocation deletes all data in the store that predate `dbLocation`.
	// If `dbPreservedLocations` is not nil - it also excludes from deletion any location specified in it.
	DeleteFromStoreUpToLocation(storeName string, dbLocation StoreLocation, dbPreservedLocations []StoreLocation) error

	// Cursor begins a new cursor over the given bucket.
	Cursor(bucket *Bucket) (Cursor, error)
}
