// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// JobEnded is the state of a job whose outcome was delivered, or that was
// replaced or ended on its leader's acceptance. A cancelled job has ended too,
// in JobCancelled. Every other state is open, a paused job's too.
const JobEnded = "ended"

// Repository is a registered repository: what `asmai repo add` recorded.
type Repository struct {
	Name string `json:"name"`
	// Location is where the user's own checkout is, or empty when the
	// repository was registered by its URL. AsmAI never uses it for work.
	Location string `json:"location"`
	// Origin is the origin remote AsmAI's clone is fetched from.
	Origin        string `json:"origin"`
	DefaultBranch string `json:"default_branch"`
	// Clone is the path of AsmAI's own clone in the state directory.
	Clone   string    `json:"clone"`
	AddedAt time.Time `json:"added_at"`
}

// ErrRepositoryRegistered is returned by RepositoryAdded for a name that is
// already registered.
var ErrRepositoryRegistered = errors.New("that name is already registered")

// OpenJobsError is returned by RepositoryRemoved for a repository that jobs
// still target.
type OpenJobsError struct {
	Repository string
	// Jobs are the numbers of its open jobs.
	Jobs []int64
}

func (e *OpenJobsError) Error() string {
	return fmt.Sprintf("%s has open jobs %v", e.Repository, e.Jobs)
}

// RepositoryAdded records that r was registered, and journals it.
func (s *Store) RepositoryAdded(r Repository) error {
	r.AddedAt = r.AddedAt.UTC()
	return s.change(r.AddedAt, func(tx *sql.Tx) (string, any, error) {
		if err := insertRepository(tx, r); err != nil {
			return "", nil, err
		}
		return KindRepositoryAdded, r, nil
	})
}

// repositoryAdded records and journals the registration of r in tx, and
// returns the journal entry.
func repositoryAdded(tx *sql.Tx, r Repository) (int64, error) {
	if err := insertRepository(tx, r); err != nil {
		return 0, err
	}
	return appendEntry(tx, r.AddedAt, KindRepositoryAdded, r)
}

func insertRepository(tx *sql.Tx, r Repository) error {
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM repositories WHERE name = ?`, r.Name).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return ErrRepositoryRegistered
	}
	_, err := tx.Exec(`INSERT INTO repositories (name, location, origin, default_branch, clone, added_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.Name, r.Location, r.Origin, r.DefaultBranch, r.Clone, timestamp(r.AddedAt))
	return err
}

// RepositoryRemoved removes the registered repository name at at, and
// journals it. It refuses, with an *OpenJobsError, while a job that is not
// ended targets the repository, and with sql.ErrNoRows when name is not
// registered.
func (s *Store) RepositoryRemoved(name string, at time.Time) (Repository, error) {
	var removed Repository
	err := s.change(at, func(tx *sql.Tx) (string, any, error) {
		r, err := repository(tx, name)
		if err != nil {
			return "", nil, err
		}
		// A cancelled job is open until its cancelling assignments end.
		rows, err := tx.Query(`SELECT number FROM jobs WHERE repository = ? AND (state NOT IN (?, ?) OR number IN (SELECT job FROM assignments WHERE state = ?)) ORDER BY number`, name, JobEnded, JobCancelled, AssignmentCancelling)
		if err != nil {
			return "", nil, err
		}
		defer rows.Close()
		var open []int64
		for rows.Next() {
			var number int64
			if err := rows.Scan(&number); err != nil {
				return "", nil, err
			}
			open = append(open, number)
		}
		if err := rows.Err(); err != nil {
			return "", nil, err
		}
		if len(open) > 0 {
			return "", nil, &OpenJobsError{Repository: name, Jobs: open}
		}
		if _, err := tx.Exec(`DELETE FROM repositories WHERE name = ?`, name); err != nil {
			return "", nil, err
		}
		// What the user answered about its setup is its own: a repository
		// registered again under the name starts without it.
		if _, err := tx.Exec(`DELETE FROM setup_answers WHERE repository = ?`, name); err != nil {
			return "", nil, err
		}
		removed = r
		return KindRepositoryRemoved, r, nil
	})
	return removed, err
}

// Repository returns the registered repository name, or sql.ErrNoRows.
func (s *Store) Repository(name string) (Repository, error) {
	return repository(s.db, name)
}

func repository(q queryer, name string) (Repository, error) {
	var r Repository
	var added string
	err := q.QueryRow(`SELECT name, location, origin, default_branch, clone, added_at FROM repositories WHERE name = ?`, name).
		Scan(&r.Name, &r.Location, &r.Origin, &r.DefaultBranch, &r.Clone, &added)
	if err != nil {
		return Repository{}, err
	}
	r.AddedAt, err = time.Parse(time.RFC3339Nano, added)
	return r, err
}

// Repositories returns the registered repositories, by name.
func (s *Store) Repositories() ([]Repository, error) {
	rows, err := s.db.Query(`SELECT name, location, origin, default_branch, clone, added_at FROM repositories ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repositories := []Repository{}
	for rows.Next() {
		var r Repository
		var added string
		if err := rows.Scan(&r.Name, &r.Location, &r.Origin, &r.DefaultBranch, &r.Clone, &added); err != nil {
			return nil, err
		}
		if r.AddedAt, err = time.Parse(time.RFC3339Nano, added); err != nil {
			return nil, err
		}
		repositories = append(repositories, r)
	}
	return repositories, rows.Err()
}
