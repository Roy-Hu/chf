package consumer

import (
	"context"
	"fmt"
	"net/http"

	"github.com/free5gc/chf/internal/logger"
	"github.com/free5gc/openapi/Nnrf_NFDiscovery"
	"github.com/free5gc/openapi/models"
)

func SendSearchNFInstances(
	nrfUri string, targetNfType, requestNfType models.NfType, param Nnrf_NFDiscovery.SearchNFInstancesParamOpts) (
	*models.SearchResult, error) {
	// Set client and set url
	configuration := Nnrf_NFDiscovery.NewConfiguration()
	configuration.SetBasePath(nrfUri)
	client := Nnrf_NFDiscovery.NewAPIClient(configuration)

	result, res, err := client.NFInstancesStoreApi.SearchNFInstances(context.TODO(), targetNfType, requestNfType, &param)
	if err != nil {
		logger.ConsumerLog.Errorf("SearchNFInstances failed: %+v", err)
	}
	defer func() {
		if resCloseErr := res.Body.Close(); resCloseErr != nil {
			logger.ConsumerLog.Errorf("NFInstancesStoreApi response body cannot close: %+v", resCloseErr)
		}
	}()
	if res != nil && res.StatusCode == http.StatusTemporaryRedirect {
		return nil, fmt.Errorf("Temporary Redirect For Non NRF Consumer")
	}

	return &result, nil
}
