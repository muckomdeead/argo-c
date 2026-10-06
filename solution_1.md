```go
package webhook_checker

import (
	"errors"
	"net/http"
)

func CheckWebhookExistence(response *http.Response) (bool, error) {
	switch response.StatusCode {
	case http.StatusNotFound:
		return false, nil
	case http.StatusTooManyRequests:
		return false, errors.New("rate limit exceeded")
	case http.StatusInternalServerError, http.StatusBadGateway, http.ServiceUnavailable:
		return false, errors.New("upstream server error")
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, errors.New("authentication/authorization issue")
	case http.StatusGatewayTimeout, http.StatusBadGateway:
		return false, errors.New("gateway timeout")
	case http.StatusConnectionClosed, http.StatusRequestTimeout:
		return false, errors.New("connection timeout")
	default:
		return true, nil
	}
}
```