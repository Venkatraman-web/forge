package worker

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// DescribeDBError turns an error from a database call into a short, fixed
// description that is safe to log. The raw error text is not logged: it comes
// from drivers and servers, can change between versions, and could in
// principle echo connection details. What operators need is kept: the kind of
// failure and, for server-side errors, the five-character SQLSTATE code, which
// can be looked up in the PostgreSQL documentation.
func DescribeDBError(err error) string {
	var pgErr interface{ SQLState() string }
	switch {
	case err == nil:
		return "no error"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &pgErr):
		return "database error (SQLSTATE " + pgErr.SQLState() + ")"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "network error"
	}
	return "database error"
}
