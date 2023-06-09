package producer

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	charging_datatype "github.com/free5gc/chf/ccs_diameter/datatype"

	"github.com/fiorix/go-diameter/diam/datatype"
	"github.com/free5gc/chf/cdr/cdrType"
	"github.com/free5gc/chf/internal/abmf"
	"github.com/free5gc/chf/internal/cgf"
	chf_context "github.com/free5gc/chf/internal/context"
	"github.com/free5gc/chf/internal/logger"
	"github.com/free5gc/chf/internal/rating"
	"github.com/free5gc/chf/internal/util"
	"github.com/free5gc/openapi/models"
	"github.com/free5gc/util/httpwrapper"
)

func NotifyRecharge(ueId string, rg int32) {
	var reauthorizationDetails []models.ReauthorizationDetails

	self := chf_context.CHF_Self()
	ue, ok := self.ChfUeFindBySupi(ueId)
	if !ok {
		logger.NotifyEventLog.Errorf("Do not find charging data for UE: %s", ueId)
		return
	}

	// If it is previosly set to debit mode due to quota exhausted, need to reverse to the reserve mode
	ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_RESERVE
	reauthorizationDetails = append(reauthorizationDetails, models.ReauthorizationDetails{
		RatingGroup: rg,
	})

	notifyRequest := models.ChargingNotifyRequest{
		ReauthorizationDetails: reauthorizationDetails,
	}

	SendChargingNotification(ue.NotifyUri, notifyRequest)
}

func SendChargingNotification(notifyUri string, notifyRequest models.ChargingNotifyRequest) {
	client := util.GetNchfChargingNotificationCallbackClient()
	logger.NotifyEventLog.Warn("Send Charging Notification  to SMF: uri: ", notifyUri)
	httpResponse, err := client.DefaultCallbackApi.ChargingNotification(context.Background(), notifyUri, notifyRequest)
	if err != nil {
		if httpResponse != nil {
			logger.NotifyEventLog.Warnf("Charging Notification Error[%s]", httpResponse.Status)
		} else {
			logger.NotifyEventLog.Warnf("Charging Notification Failed[%s]", err.Error())
		}
		return
	} else if httpResponse == nil {
		logger.NotifyEventLog.Warnln("Charging Notification[HTTP Response is nil]")
		return
	}
	defer func() {
		if resCloseErr := httpResponse.Body.Close(); resCloseErr != nil {
			logger.NotifyEventLog.Errorf("NFInstancesStoreApi response body cannot close: %+v", resCloseErr)
		}
	}()
	if httpResponse.StatusCode != http.StatusOK && httpResponse.StatusCode != http.StatusNoContent {
		logger.NotifyEventLog.Warnf("Charging Notification Failed")
	} else {
		logger.NotifyEventLog.Tracef("Charging Notification Success")
	}
}

func HandleChargingdataInitial(request *httpwrapper.Request) *httpwrapper.Response {
	logger.ChargingdataPostLog.Infof("HandleChargingdataInitial")
	chargingdata := request.Body.(models.ChargingDataRequest)

	response, locationURI, problemDetails := ChargingDataCreate(chargingdata)
	respHeader := make(http.Header)
	respHeader.Set("Location", locationURI)

	if response != nil {
		return httpwrapper.NewResponse(http.StatusCreated, respHeader, response)
	} else if problemDetails != nil {
		return httpwrapper.NewResponse(int(problemDetails.Status), nil, problemDetails)
	}
	problemDetails = &models.ProblemDetails{
		Status: http.StatusForbidden,
		Cause:  "UNSPECIFIED",
	}
	return httpwrapper.NewResponse(http.StatusForbidden, nil, problemDetails)
}

func HandleChargingdataUpdate(request *httpwrapper.Request) *httpwrapper.Response {
	logger.ChargingdataPostLog.Infof("HandleChargingdataUpdate")
	chargingdata := request.Body.(models.ChargingDataRequest)
	chargingSessionId := request.Params["ChargingDataRef"]

	response, problemDetails := ChargingDataUpdate(chargingdata, chargingSessionId)

	if response != nil {
		return httpwrapper.NewResponse(http.StatusOK, nil, response)
	} else if problemDetails != nil {
		return httpwrapper.NewResponse(int(problemDetails.Status), nil, problemDetails)
	}
	problemDetails = &models.ProblemDetails{
		Status: http.StatusForbidden,
		Cause:  "UNSPECIFIED",
	}
	return httpwrapper.NewResponse(http.StatusForbidden, nil, problemDetails)
}

func HandleChargingdataRelease(request *httpwrapper.Request) *httpwrapper.Response {
	logger.ChargingdataPostLog.Infof("HandleChargingdateRelease")
	chargingdata := request.Body.(models.ChargingDataRequest)
	chargingSessionId := request.Params["ChargingDataRef"]

	problemDetails := ChargingDataRelease(chargingdata, chargingSessionId)

	if problemDetails == nil {
		return httpwrapper.NewResponse(http.StatusNoContent, nil, nil)
	} else if problemDetails != nil {
		return httpwrapper.NewResponse(int(problemDetails.Status), nil, problemDetails)
	}
	problemDetails = &models.ProblemDetails{
		Status: http.StatusForbidden,
		Cause:  "UNSPECIFIED",
	}
	return httpwrapper.NewResponse(http.StatusForbidden, nil, problemDetails)
}

func ChargingDataCreate(chargingData models.ChargingDataRequest) (*models.ChargingDataResponse,
	string, *models.ProblemDetails) {
	var responseBody models.ChargingDataResponse
	var chargingSessionId string

	self := chf_context.CHF_Self()
	ueId := chargingData.SubscriberIdentifier

	// Open CDR
	// ChargingDataRef(charging session id):
	// A unique identifier for a charging data resource in a PLMN
	// TODO determine charging session id(string type) supi+consumerid+localseq?
	ue, err := self.NewCHFUe(ueId)
	if err != nil {
		logger.ChargingdataPostLog.Errorf("New CHFUe error %s", err)
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, "", problemDetails
	}

	ue.CULock.Lock()
	defer ue.CULock.Unlock()

	ue.NotifyUri = chargingData.NotifyUri

	consumerId := chargingData.NfConsumerIdentification.NFName
	if !chargingData.OneTimeEvent {
		chargingSessionId = ueId + consumerId + strconv.Itoa(int(self.LocalRecordSequenceNumber))
	}
	cdr, err := OpenCDR(chargingData, ue, chargingSessionId, false)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, "", problemDetails
	}

	err = UpdateCDR(cdr, chargingData)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, "", problemDetails
	}

	ue.Cdr[chargingSessionId] = cdr

	if chargingData.OneTimeEvent {
		err = CloseCDR(cdr, false)
		if err != nil {
			problemDetails := &models.ProblemDetails{
				Status: http.StatusBadRequest,
			}
			return nil, "", problemDetails
		}
	}

	// CDR Transfer
	err = cgf.SendCDR(chargingData.SubscriberIdentifier)
	if err != nil {
		logger.ChargingdataPostLog.Errorf("Charging gateway fail to send CDR to billing domain %v", err)
	}

	logger.ChargingdataPostLog.Infof("Open CDR for UE %s", ueId)

	// build response
	logger.ChargingdataPostLog.Infof("NewChfUe %s", ueId)
	locationURI := self.GetIPv4Uri() + "/nchf-convergedcharging/v3/chargingdata/" + chargingSessionId
	timeStamp := time.Now()

	responseBody.InvocationTimeStamp = &timeStamp
	responseBody.InvocationSequenceNumber = chargingData.InvocationSequenceNumber

	return &responseBody, locationURI, nil
}

func ChargingDataUpdate(chargingData models.ChargingDataRequest, chargingSessionId string) (*models.ChargingDataResponse,
	*models.ProblemDetails) {
	var records []*cdrType.CHFRecord

	self := chf_context.CHF_Self()
	ueId := chargingData.SubscriberIdentifier
	ue, ok := self.ChfUeFindBySupi(ueId)
	if !ok {
		logger.ChargingdataPostLog.Errorf("Do not find  CHFUe[%s] error", ueId)
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, problemDetails
	}

	ue.CULock.Lock()
	defer ue.CULock.Unlock()

	// Online charging: Rate, Account, Reservation
	responseBody, partialRecord := BuildOnlineChargingDataUpdateResopone(chargingData)

	cdr := ue.Cdr[chargingSessionId]
	err := UpdateCDR(cdr, chargingData)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, problemDetails
	}

	if partialRecord {
		ueId := chargingData.SubscriberIdentifier

		CloseCDR(cdr, partialRecord)
		err = dumpCdrFile(ueId, []*cdrType.CHFRecord{cdr})
		if err != nil {
			problemDetails := &models.ProblemDetails{
				Status: http.StatusBadRequest,
			}
			return nil, problemDetails
		}

		OpenCDR(chargingData, ue, chargingSessionId, partialRecord)
		logger.ChargingdataPostLog.Tracef("CDR Record Sequence Number after Reopen %+v", *cdr.ChargingFunctionRecord.RecordSequenceNumber)
	}

	for _, cdr := range ue.Cdr {
		records = append(records, cdr)
	}
	err = dumpCdrFile(ueId, records)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return nil, problemDetails
	}

	err = cgf.SendCDR(chargingData.SubscriberIdentifier)
	if err != nil {
		logger.ChargingdataPostLog.Errorf("Charging gateway fail to send CDR to billing domain %v", err)
	}

	timeStamp := time.Now()
	responseBody.InvocationTimeStamp = &timeStamp
	responseBody.InvocationSequenceNumber = chargingData.InvocationSequenceNumber

	return &responseBody, nil
}

func ChargingDataRelease(chargingData models.ChargingDataRequest, chargingSessionId string) *models.ProblemDetails {
	self := chf_context.CHF_Self()
	ueId := chargingData.SubscriberIdentifier
	ue, ok := self.ChfUeFindBySupi(ueId)
	if !ok {
		logger.ChargingdataPostLog.Errorf("Do not find CHFUe[%s] error", ueId)
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	ue.CULock.Lock()
	defer ue.CULock.Unlock()

	sessionChargingReservation(chargingData)

	cdr := ue.Cdr[chargingSessionId]

	err := UpdateCDR(cdr, chargingData)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	err = CloseCDR(cdr, false)
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	err = dumpCdrFile(ueId, []*cdrType.CHFRecord{cdr})
	if err != nil {
		problemDetails := &models.ProblemDetails{
			Status: http.StatusBadRequest,
		}
		return problemDetails
	}

	return nil
}

func BuildOnlineChargingDataCreateResopone(ue *chf_context.ChfUe, chargingData models.ChargingDataRequest) models.ChargingDataResponse {
	logger.ChargingdataPostLog.Info("In Build Online Charging Data Create Resopone")
	ue.NotifyUri = chargingData.NotifyUri

	multipleUnitInformation, _ := sessionChargingReservation(chargingData)

	responseBody := models.ChargingDataResponse{
		MultipleUnitInformation: multipleUnitInformation,
	}

	return responseBody
}

func BuildOnlineChargingDataUpdateResopone(chargingData models.ChargingDataRequest) (models.ChargingDataResponse, bool) {
	var partialRecord bool

	logger.ChargingdataPostLog.Info("In BuildOnlineChargingDataUpdateResopone ")

	multipleUnitInformation, partialRecord := sessionChargingReservation(chargingData)

	responseBody := models.ChargingDataResponse{
		MultipleUnitInformation: multipleUnitInformation,
	}

	return responseBody, partialRecord
}

// 32.296 6.2.2.3.1: Service usage request method with reservation
func sessionChargingReservation(chargingData models.ChargingDataRequest) ([]models.MultipleUnitInformation, bool) {
	var multipleUnitInformation []models.MultipleUnitInformation
	var partialRecord bool
	var subscriberIdentifier *charging_datatype.SubscriptionId

	self := chf_context.CHF_Self()
	supi := chargingData.SubscriberIdentifier

	ue, ok := self.ChfUeFindBySupi(supi)
	if !ok {
		logger.ChargingdataPostLog.Warnf("Do not find UE[%s]", supi)
		return nil, false
	}

	supiType := strings.Split(supi, "-")[0]
	switch supiType {
	case "imsi":
		subscriberIdentifier = &charging_datatype.SubscriptionId{
			SubscriptionIdType: charging_datatype.END_USER_IMSI,
			SubscriptionIdData: datatype.UTF8String(supi[5:]),
		}
	case "nai":
		subscriberIdentifier = &charging_datatype.SubscriptionId{
			SubscriptionIdType: charging_datatype.END_USER_NAI,
			SubscriptionIdData: datatype.UTF8String(supi[4:]),
		}
	case "gci":
		subscriberIdentifier = &charging_datatype.SubscriptionId{
			SubscriptionIdType: charging_datatype.END_USER_NAI,
			SubscriptionIdData: datatype.UTF8String(supi[4:]),
		}
	case "gli":
		subscriberIdentifier = &charging_datatype.SubscriptionId{
			SubscriptionIdType: charging_datatype.END_USER_NAI,
			SubscriptionIdData: datatype.UTF8String(supi[4:]),
		}
	}

	for unitUsageNum, unitUsage := range chargingData.MultipleUnitUsage {
		var totalUsaedUnit uint32
		var finalUnitIndication models.FinalUnitIndication
		offline := true

		rg := unitUsage.RatingGroup
		if !ue.FindRatingGroup(rg) {
			ue.RatingGroups = append(ue.RatingGroups, rg)
			ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_RESERVE
		}

		for _, useduint := range unitUsage.UsedUnitContainer {
			switch useduint.QuotaManagementIndicator {
			case models.QuotaManagementIndicator_OFFLINE_CHARGING:
				continue
			case models.QuotaManagementIndicator_ONLINE_CHARGING:
				offline = false
				for _, trigger := range chargingData.Triggers {
					// Check if partial record is needed
					if trigger.TriggerType == models.TriggerType_VOLUME_LIMIT &&
						trigger.TriggerCategory == models.TriggerCategory_IMMEDIATE_REPORT {
						partialRecord = true
					}
					if trigger.TriggerType == models.TriggerType_FINAL {
						ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_DEBIT
					}
				}
				// calculate total used unit
				totalUsaedUnit += uint32(useduint.TotalVolume)
			case models.QuotaManagementIndicator_QUOTA_MANAGEMENT_SUSPENDED:
				logger.ChargingdataPostLog.Errorf("Current do not support QUOTA MANAGEMENT SUSPENDED")
			}
		}
		if offline {
			continue
		}
		// Only online charging need to perform credit control

		ccr := &charging_datatype.AccountDebitRequest{
			SessionId:       datatype.UTF8String(strconv.Itoa(int(ue.AcctSessionId))),
			OriginHost:      datatype.DiameterIdentity(self.AbmfCfg.OriginHost),
			OriginRealm:     datatype.DiameterIdentity(self.AbmfCfg.OriginRealm),
			EventTimestamp:  datatype.Time(time.Now()),
			SubscriptionId:  subscriberIdentifier,
			UserName:        datatype.OctetString(self.Name),
			CcRequestNumber: datatype.Unsigned32(ue.AcctRequestNum[rg]),
		}

		sur := &charging_datatype.ServiceUsageRequest{
			SessionId:      datatype.UTF8String(strconv.Itoa(int(ue.RateSessionId))),
			OriginHost:     datatype.DiameterIdentity(self.RatingCfg.OriginHost),
			OriginRealm:    datatype.DiameterIdentity(self.RatingCfg.OriginRealm),
			ActualTime:     datatype.Time(time.Now()),
			SubscriptionId: subscriberIdentifier,
			UserName:       datatype.OctetString(self.Name),
		}

		unitInformation := models.MultipleUnitInformation{
			UPFID:               unitUsage.UPFID,
			FinalUnitIndication: &finalUnitIndication,
			RatingGroup:         rg,
		}

		switch ue.RatingType[rg] {
		case charging_datatype.REQ_SUBTYPE_RESERVE:
			if ue.ReservedQuota[rg] == 0 {
				ccr.CcRequestType = charging_datatype.UPDATE_REQUEST
				ccr.RequestedAction = charging_datatype.DIRECT_DEBITING
				ccr.MultipleServicesCreditControl = &charging_datatype.MultipleServicesCreditControl{
					RatingGroup: datatype.Unsigned32(rg),
					RequestedServiceUnit: &charging_datatype.RequestedServiceUnit{
						CCTotalOctets: datatype.Unsigned64(unitUsage.RequestedUnit.TotalVolume * 10),
					},
				}
			} else {
				ccr.CcRequestType = charging_datatype.UPDATE_REQUEST
				ccr.RequestedAction = charging_datatype.DIRECT_DEBITING
				logger.ChargingdataPostLog.Tracef("UsedUnit %v UnitCost: %v", totalUsaedUnit, ue.UnitCost[rg])

				prevReserved := ue.ReservedQuota[rg]
				usedQuota := int64(totalUsaedUnit * ue.UnitCost[rg])
				ue.ReservedQuota[rg] -= usedQuota
				insufficient := usedQuota - prevReserved

				requestedQuota := int64(unitUsage.RequestedUnit.TotalVolume*int32(ue.UnitCost[rg])) + insufficient

				ccr.MultipleServicesCreditControl = &charging_datatype.MultipleServicesCreditControl{
					RatingGroup: datatype.Unsigned32(rg),
					RequestedServiceUnit: &charging_datatype.RequestedServiceUnit{
						CCTotalOctets: datatype.Unsigned64(requestedQuota),
					},
				}
			}

			acctDebitRsp, err := abmf.SendAccountDebitRequest(ue, ccr)
			if err != nil {
				logger.ChargingdataPostLog.Errorf("SendAccountDebitRequest err: %+v", err)
				continue
			}
			logger.ChargingdataPostLog.Tracef("reserved Unit: %d", acctDebitRsp.MultipleServicesCreditControl.GrantedServiceUnit.CCTotalOctets)

			ue.ReservedQuota[rg] += int64(acctDebitRsp.MultipleServicesCreditControl.GrantedServiceUnit.CCTotalOctets)

			// Deduict the reserved quota from the account
			if acctDebitRsp.MultipleServicesCreditControl.FinalUnitIndication != nil {
				switch acctDebitRsp.MultipleServicesCreditControl.FinalUnitIndication.FinalUnitAction {
				case charging_datatype.TERMINATE:
					logger.ChargingdataPostLog.Warnf("Last granted quota")
					finalUnitIndication = models.FinalUnitIndication{
						FinalUnitAction: models.FinalUnitAction_TERMINATE,
					}
					ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_DEBIT
				}
			}

			sur.ServiceRating = &charging_datatype.ServiceRating{
				ServiceIdentifier: datatype.Unsigned32(rg),
				MonetaryQuota:     datatype.Unsigned32(ue.ReservedQuota[rg]),
				RequestSubType:    charging_datatype.REQ_SUBTYPE_RESERVE,
			}

			// Retrieve and save the tarrif for pricing the next usage
			serviceUsageRsp, err := rating.SendServiceUsageRequest(ue, sur)
			if err != nil {
				logger.ChargingdataPostLog.Errorf("SendServiceUsageRequest err: %+v", err)
				continue
			}

			ue.UnitCost[rg] = uint32(serviceUsageRsp.ServiceRating.MonetaryTariff.RateElement.UnitCost.ValueDigits) *
				uint32(math.Pow10(int(serviceUsageRsp.ServiceRating.MonetaryTariff.RateElement.UnitCost.Exponent)))

			grantedUnit := uint32(serviceUsageRsp.ServiceRating.AllowedUnits)
			logger.ChargingdataPostLog.Tracef("granted Unit: %d", grantedUnit)

			if ue.RatingType[rg] == charging_datatype.REQ_SUBTYPE_RESERVE {
				unitInformation.VolumeQuotaThreshold = int32(float32(grantedUnit) * ue.VolumeThresholdRate)
			}

			unitInformation.GrantedUnit = &models.GrantedUnit{
				TotalVolume:    int32(grantedUnit),
				DownlinkVolume: int32(grantedUnit),
				UplinkVolume:   int32(grantedUnit),
			}

			// The timer of VolumeLimit is remain in SMF
			if ue.VolumeLimit != 0 {
				unitInformation.Triggers = append(unitInformation.Triggers,
					models.Trigger{
						TriggerType:     models.TriggerType_VOLUME_LIMIT,
						TriggerCategory: models.TriggerCategory_DEFERRED_REPORT,
						VolumeLimit:     ue.VolumeLimit,
					},
				)
			}

			// VolumeLimit for PDU session only need to add once
			if ue.VolumeLimitPDU != 0 && unitUsageNum == 0 {
				unitInformation.Triggers = append(unitInformation.Triggers,
					models.Trigger{
						TriggerType:     models.TriggerType_VOLUME_LIMIT,
						TriggerCategory: models.TriggerCategory_IMMEDIATE_REPORT,
						VolumeLimit:     ue.VolumeLimitPDU,
					},
				)
			}

			// The timer of QuotaValidityTime is remain in UPF
			if ue.QuotaValidityTime != 0 {
				unitInformation.ValidityTime = ue.QuotaValidityTime
			}

		case charging_datatype.REQ_SUBTYPE_DEBIT:
			logger.ChargingdataPostLog.Warnf("Debit mode, will not further grant unit")
			// retrived tarrif for final pricing
			sur.ServiceRating = &charging_datatype.ServiceRating{
				ServiceIdentifier: datatype.Unsigned32(rg),
				ConsumedUnits:     datatype.Unsigned32(totalUsaedUnit),
				RequestSubType:    charging_datatype.REQ_SUBTYPE_DEBIT,
			}

			serviceUsageRsp, err := rating.SendServiceUsageRequest(ue, sur)
			if err != nil {
				logger.ChargingdataPostLog.Errorf("SendServiceUsageRequest err: %+v", err)
				continue
			}

			if int64(serviceUsageRsp.ServiceRating.Price) < ue.ReservedQuota[rg] {
				// The final consumed quota is smaller than the reserved quota
				// Therefore, return the extra reserved quota back to the user account
				reservedRemained := ue.ReservedQuota[rg] - int64(serviceUsageRsp.ServiceRating.Price)
				ccr.RequestedAction = charging_datatype.REFUND_ACCOUNT
				ccr.MultipleServicesCreditControl = &charging_datatype.MultipleServicesCreditControl{
					RatingGroup: datatype.Unsigned32(rg),
					RequestedServiceUnit: &charging_datatype.RequestedServiceUnit{
						CCTotalOctets: datatype.Unsigned64(reservedRemained),
					},
				}
				// Typically, the reserved quota will be exhausted for the flow (or PDU session)
				// However, for the case the flow quota  and PDU session's quota is both last granted quota
				// and the PDU session's quota is larger than the flow's quota
				// PDU session's quota should be refund and set to reserved mode in order to reserve the quota for other flow
				ue.RatingType[rg] = charging_datatype.REQ_SUBTYPE_RESERVE
			} else {
				// The final consumed quota exceed the reserved quota
				// Deduct the extra consumed quota from the user account
				extraConsumed := int64(serviceUsageRsp.ServiceRating.Price) - ue.ReservedQuota[rg]
				ccr.RequestedAction = charging_datatype.DIRECT_DEBITING
				ccr.CcRequestType = charging_datatype.TERMINATION_REQUEST
				ccr.MultipleServicesCreditControl = &charging_datatype.MultipleServicesCreditControl{
					RatingGroup: datatype.Unsigned32(rg),
					UsedServiceUnit: &charging_datatype.UsedServiceUnit{
						CCTotalOctets: datatype.Unsigned64(extraConsumed),
					},
				}
			}

			_, err = abmf.SendAccountDebitRequest(ue, ccr)
			if err != nil {
				logger.ChargingdataPostLog.Errorf("SendAccountDebitRequest err: %+v", err)
				continue
			}
			ue.ReservedQuota[rg] = 0
		}
		multipleUnitInformation = append(multipleUnitInformation, unitInformation)

		ue.AcctRequestNum[rg]++
	}

	return multipleUnitInformation, partialRecord
}
