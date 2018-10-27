package oci8

// #include "oci8.go.h"
import "C"

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

// ParseDSN parses a DSN used to connect to Oracle
// It expects to receive a string in the form:
// [username/[password]@]host[:port][/instance_name][?param1=value1&...&paramN=valueN]
//
// Currently the parameters supported is:
// 1 'loc' which
// sets the timezone to read times in as and to marshal to when writing times to
// Oracle date,
// 2 'isolation' =READONLY,SERIALIZABLE,DEFAULT
// 3 'prefetch_rows'
// 4 'prefetch_memory'
// 5 'questionph' =YES,NO,TRUE,FALSE enable question-mark placeholders, default to false
func ParseDSN(dsnString string) (dsn *DSN, err error) {

	dsn = &DSN{Location: time.Local}

	if dsnString == "" {
		return nil, errors.New("empty dsn")
	}

	const prefix = "oracle://"

	if strings.HasPrefix(dsnString, prefix) {
		dsnString = dsnString[len(prefix):]
	}

	authority, dsnString := splitRight(dsnString, "@")
	if authority != "" {
		dsn.Username, dsn.Password, err = parseAuthority(authority)
		if err != nil {
			return nil, err
		}
	}

	host, params := splitRight(dsnString, "?")

	if host, err = unescape(host, encodeHost); err != nil {
		return nil, err
	}

	dsn.Connect = host

	// set safe defaults
	dsn.prefetchRows = 10
	dsn.prefetchMemory = 0
	dsn.operationMode = C.OCI_DEFAULT

	qp, err := ParseQuery(params)
	for k, v := range qp {
		switch k {
		case "loc":
			if len(v) > 0 {
				if dsn.Location, err = time.LoadLocation(v[0]); err != nil {
					return nil, fmt.Errorf("Invalid loc: %v: %v", v[0], err)
				}
			}
		case "isolation":
			switch v[0] {
			case "READONLY":
				dsn.transactionMode = C.OCI_TRANS_READONLY
			case "SERIALIZABLE":
				dsn.transactionMode = C.OCI_TRANS_SERIALIZABLE
			case "DEFAULT":
				dsn.transactionMode = C.OCI_TRANS_READWRITE
			default:
				return nil, fmt.Errorf("Invalid isolation: %v", v[0])
			}
		case "questionph":
			switch v[0] {
			case "YES", "TRUE":
				dsn.enableQMPlaceholders = true
			case "NO", "FALSE":
				dsn.enableQMPlaceholders = false
			default:
				return nil, fmt.Errorf("Invalid questionpm: %v", v[0])
			}
		case "prefetch_rows":
			z, err := strconv.ParseUint(v[0], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid prefetch_rows: %v", v[0])
			}
			dsn.prefetchRows = uint32(z)
		case "prefetch_memory":
			z, err := strconv.ParseUint(v[0], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid prefetch_memory: %v", v[0])
			}
			dsn.prefetchMemory = uint32(z)
		case "as":
			switch v[0] {
			case "SYSDBA", "sysdba":
				dsn.operationMode = C.OCI_SYSDBA
			case "SYSASM", "sysasm":
				dsn.operationMode = C.OCI_SYSASM
			case "SYSOPER", "sysoper":
				dsn.operationMode = C.OCI_SYSOPER
			default:
				return nil, fmt.Errorf("Invalid as: %v", v[0])
			}

		}
	}

	if len(dsn.Username)+len(dsn.Password)+len(dsn.Connect) == 0 {
		dsn.externalauthentication = true
	}
	return dsn, nil
}

// Commit transaction commit
func (tx *OCI8Tx) Commit() error {
	tx.conn.inTransaction = false
	if rv := C.OCITransCommit(
		tx.conn.svc,
		tx.conn.errHandle,
		0); rv != C.OCI_SUCCESS {
		return tx.conn.getError(rv)
	}
	return nil
}

// Rollback transaction rollback
func (tx *OCI8Tx) Rollback() error {
	tx.conn.inTransaction = false
	if rv := C.OCITransRollback(
		tx.conn.svc,
		tx.conn.errHandle,
		0); rv != C.OCI_SUCCESS {
		return tx.conn.getError(rv)
	}
	return nil
}

// Open opens a new database connection
func (oci8Driver *OCI8DriverStruct) Open(dsnString string) (connection driver.Conn, err error) {
	var dsn *DSN
	if dsn, err = ParseDSN(dsnString); err != nil {
		return
	}

	conn := OCI8Conn{
		operationMode: dsn.operationMode,
		logger:        oci8Driver.Logger,
	}
	if conn.logger == nil {
		conn.logger = log.New(ioutil.Discard, "", 0)
	}

	if rv := C.WrapOCIEnvCreate(
		C.OCI_DEFAULT|C.OCI_THREADED,
		0,
	); rv.rv != C.OCI_SUCCESS && rv.rv != C.OCI_SUCCESS_WITH_INFO {
		// TODO: error handle not yet allocated, we can't get string error from oracle
		err = errors.New("can't OCIEnvCreate")
		return
	} else {
		conn.env = (*C.OCIEnv)(rv.ptr)
		// conn allocations: env
	}

	if rv := C.WrapOCIHandleAlloc(
		unsafe.Pointer(conn.env),
		C.OCI_HTYPE_ERROR,
		0,
	); rv.rv != C.OCI_SUCCESS {
		err = errors.New("cant allocate error handle")
		C.OCIHandleFree(unsafe.Pointer(conn.env), C.OCI_HTYPE_ENV)
		return
	} else {
		conn.errHandle = (*C.OCIError)(rv.ptr)
		// conn allocations: env, err
	}

	phost := C.CString(dsn.Connect)
	defer C.free(unsafe.Pointer(phost))
	puser := C.CString(dsn.Username)
	defer C.free(unsafe.Pointer(puser))
	ppass := C.CString(dsn.Password)
	defer C.free(unsafe.Pointer(ppass))

	if useOCISessionBegin {
		if rv := C.WrapOCIHandleAlloc(
			unsafe.Pointer(conn.env),
			C.OCI_HTYPE_SERVER,
			0,
		); rv.rv != C.OCI_SUCCESS {
			err = errors.New("cant allocate server handle")
			C.OCIHandleFree(unsafe.Pointer(conn.errHandle), C.OCI_HTYPE_ERROR)
			C.OCIHandleFree(unsafe.Pointer(conn.env), C.OCI_HTYPE_ENV)
			return
		} else {
			conn.srv = (*C.OCIServer)(rv.ptr)
			// conn allocations: env, err, srv
		}

		if dsn.externalauthentication {
			C.WrapOCIServerAttach(
				conn.srv,
				conn.errHandle,
				nil,
				0,
				C.OCI_DEFAULT)
		} else {
			C.WrapOCIServerAttach(
				conn.srv,
				conn.errHandle,
				(*C.text)(unsafe.Pointer(phost)),
				C.ub4(len(dsn.Connect)),
				C.OCI_DEFAULT)
		}

		if rv := C.WrapOCIHandleAlloc(
			unsafe.Pointer(conn.env),
			C.OCI_HTYPE_SVCCTX,
			0,
		); rv.rv != C.OCI_SUCCESS {
			err = errors.New("cant allocate service handle")
			C.OCIHandleFree(unsafe.Pointer(conn.srv), C.OCI_HTYPE_SERVER)
			C.OCIHandleFree(unsafe.Pointer(conn.errHandle), C.OCI_HTYPE_ERROR)
			C.OCIHandleFree(unsafe.Pointer(conn.env), C.OCI_HTYPE_ENV)
			return
		} else {
			conn.svc = (*C.OCISvcCtx)(rv.ptr)
			// conn allocations: env, err, srv, svc
		}

		if rv := C.OCIAttrSet(
			unsafe.Pointer(conn.svc),
			C.OCI_HTYPE_SVCCTX,
			unsafe.Pointer(conn.srv),
			0,
			C.OCI_ATTR_SERVER,
			conn.errHandle,
		); rv != C.OCI_SUCCESS {
			err = conn.getError(rv)
			C.OCIHandleFree(unsafe.Pointer(conn.svc), C.OCI_HTYPE_SVCCTX)
			C.OCIHandleFree(unsafe.Pointer(conn.srv), C.OCI_HTYPE_SERVER)
			C.OCIHandleFree(unsafe.Pointer(conn.errHandle), C.OCI_HTYPE_ERROR)
			C.OCIHandleFree(unsafe.Pointer(conn.env), C.OCI_HTYPE_ENV)
			return
		}

		// allocate a user session handle
		if rv := C.WrapOCIHandleAlloc(
			unsafe.Pointer(conn.env),
			C.OCI_HTYPE_SESSION,
			0,
		); rv.rv != C.OCI_SUCCESS {
			err = errors.New("cant allocate user session handle")
			C.OCIHandleFree(unsafe.Pointer(conn.svc), C.OCI_HTYPE_SVCCTX)
			C.OCIHandleFree(unsafe.Pointer(conn.srv), C.OCI_HTYPE_SERVER)
			C.OCIHandleFree(unsafe.Pointer(conn.errHandle), C.OCI_HTYPE_ERROR)
			C.OCIHandleFree(unsafe.Pointer(conn.env), C.OCI_HTYPE_ENV)
			return
		} else {
			conn.usrSession = (*C.OCISession)(rv.ptr)
			// conn allocations: env, err, srv, svc, usrSession
		}

		if !dsn.externalauthentication {
			//  set username attribute in user session handle
			if rv := C.OCIAttrSet(
				unsafe.Pointer(conn.usrSession),
				C.OCI_HTYPE_SESSION,
				(unsafe.Pointer(puser)),
				C.ub4(len(dsn.Username)),
				C.OCI_ATTR_USERNAME,
				conn.errHandle,
			); rv != C.OCI_SUCCESS {
				err = conn.getError(rv)
				C.OCIHandleFree(unsafe.Pointer(conn.usrSession), C.OCI_HTYPE_SESSION)
				C.OCIHandleFree(unsafe.Pointer(conn.svc), C.OCI_HTYPE_SVCCTX)
				C.OCIHandleFree(unsafe.Pointer(conn.srv), C.OCI_HTYPE_SERVER)
				C.OCIHandleFree(unsafe.Pointer(conn.errHandle), C.OCI_HTYPE_ERROR)
				C.OCIHandleFree(unsafe.Pointer(conn.env), C.OCI_HTYPE_ENV)
				return
			}

			// set password attribute in the user session handle
			if rv := C.OCIAttrSet(
				unsafe.Pointer(conn.usrSession),
				C.OCI_HTYPE_SESSION,
				(unsafe.Pointer(ppass)),
				C.ub4(len(dsn.Password)),
				C.OCI_ATTR_PASSWORD,
				conn.errHandle,
			); rv != C.OCI_SUCCESS {
				err = conn.getError(rv)
				C.OCIHandleFree(unsafe.Pointer(conn.usrSession), C.OCI_HTYPE_SESSION)
				C.OCIHandleFree(unsafe.Pointer(conn.svc), C.OCI_HTYPE_SVCCTX)
				C.OCIHandleFree(unsafe.Pointer(conn.srv), C.OCI_HTYPE_SERVER)
				C.OCIHandleFree(unsafe.Pointer(conn.errHandle), C.OCI_HTYPE_ERROR)
				C.OCIHandleFree(unsafe.Pointer(conn.env), C.OCI_HTYPE_ENV)
				return
			}

			// begin the session
			C.WrapOCISessionBegin(
				conn.svc,
				conn.errHandle,
				conn.usrSession,
				C.OCI_CRED_RDBMS,
				conn.operationMode)
		} else {
			// external authentication
			C.WrapOCISessionBegin(
				conn.svc,
				conn.errHandle,
				conn.usrSession,
				C.OCI_CRED_EXT,
				conn.operationMode)
		}

		// set the user session attribute in the service context handle
		if rv := C.OCIAttrSet(
			unsafe.Pointer(conn.svc),
			C.OCI_HTYPE_SVCCTX,
			unsafe.Pointer(conn.usrSession),
			0,
			C.OCI_ATTR_SESSION,
			conn.errHandle,
		); rv != C.OCI_SUCCESS {
			err = conn.getError(rv)
			C.OCIHandleFree(unsafe.Pointer(conn.usrSession), C.OCI_HTYPE_SESSION)
			C.OCIHandleFree(unsafe.Pointer(conn.svc), C.OCI_HTYPE_SVCCTX)
			C.OCIHandleFree(unsafe.Pointer(conn.srv), C.OCI_HTYPE_SERVER)
			C.OCIHandleFree(unsafe.Pointer(conn.errHandle), C.OCI_HTYPE_ERROR)
			C.OCIHandleFree(unsafe.Pointer(conn.env), C.OCI_HTYPE_ENV)
			return
		}

	} else {
		if rv := C.WrapOCILogon(
			conn.env,
			conn.errHandle,
			(*C.OraText)(unsafe.Pointer(puser)),
			C.ub4(len(dsn.Username)),
			(*C.OraText)(unsafe.Pointer(ppass)),
			C.ub4(len(dsn.Password)),
			(*C.OraText)(unsafe.Pointer(phost)),
			C.ub4(len(dsn.Connect)),
		); rv.rv != C.OCI_SUCCESS && rv.rv != C.OCI_SUCCESS_WITH_INFO {
			err = conn.getError(rv.rv)
			C.OCIHandleFree(unsafe.Pointer(conn.errHandle), C.OCI_HTYPE_ERROR)
			C.OCIHandleFree(unsafe.Pointer(conn.env), C.OCI_HTYPE_ENV)
			return
		} else {
			conn.svc = (*C.OCISvcCtx)(rv.ptr)
			// conn allocations: env, err
		}

	}

	conn.location = dsn.Location
	conn.transactionMode = dsn.transactionMode
	conn.prefetchRows = dsn.prefetchRows
	conn.prefetchMemory = dsn.prefetchMemory
	conn.enableQMPlaceholders = dsn.enableQMPlaceholders

	connection = &conn

	return
}

// GetLastInsertId retuns last inserted ID
func GetLastInsertId(id int64) string {
	return *(*string)(unsafe.Pointer(uintptr(id)))
}

// LastInsertId returns last inserted ID
func (r *OCI8Result) LastInsertId() (int64, error) {
	return r.id, r.errid
}

// RowsAffected returns rows affected
func (r *OCI8Result) RowsAffected() (int64, error) {
	return r.n, r.errn
}

// converts "?" characters to  :1, :2, ... :n
func placeholders(sql string) string {
	n := 0
	return phre.ReplaceAllStringFunc(sql, func(string) string {
		n++
		return ":" + strconv.Itoa(n)
	})
}
