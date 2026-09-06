package singlestore

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Driver messages stay within the backend. Only named conflicts become domain errors.
func translateBackendConflict(err error) error {
	if err == nil {
		return nil
	}
	var driver *mysql.MySQLError
	if !errors.As(err, &driver) {
		return err
	}
	switch driver.Number {
	case 1062:
		for _, key := range []string{"jobs_pkey", "jobs_dispatch_unique"} {
			if strings.Contains(driver.Message, key) {
				return store.ErrDispatchJobConflict
			}
		}
		if strings.Contains(driver.Message, "reference_documents_live_name_idx") {
			return store.ErrReferenceDocumentNameConflict
		}
	case 1205, 1213:
		return store.ErrRetryable
	}
	return fmt.Errorf("SingleStore database operation failed")
}
