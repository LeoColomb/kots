package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/replicatedhq/kots/kotsadm/pkg/logger"
	"github.com/replicatedhq/kots/kotsadm/pkg/store"
)

type SetPrometheusAddressRequest struct {
	Value string `json:"value"`
}

func SetPrometheusAddress(w http.ResponseWriter, r *http.Request) {
	if handleOptionsRequest(w, r) {
		return
	}

	if err := requireValidSession(w, r); err != nil {
		logger.Error(err)
		return
	}

	setPrometheusAddressRequest := SetPrometheusAddressRequest{}
	if err := json.NewDecoder(r.Body).Decode(&setPrometheusAddressRequest); err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	if err := store.GetStore().SetPrometheusAddress(setPrometheusAddressRequest.Value); err != nil {
		logger.Error(err)
		w.WriteHeader(500)
		return
	}

	JSON(w, 204, "")
}
