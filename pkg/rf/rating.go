// Copyright 2013-2015 go-diameter authors.  All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Diameter server example. This is by no means a complete server.
//
// If you'd like to test diameter over SSL, generate SSL certificates:
//   go run $GOROOT/src/crypto/tls/generate_cert.go --host localhost
//
// And start the server with `-cert_file cert.pem -key_file key.pem`.
//
// By default this server runs in a single OS thread. If you want to
// make it run on more, set the GOMAXPROCS=n environment variable.
// See Go's FAQ for details: http://golang.org/doc/faq#Why_no_multi_CPU

package rf

import (
	"bytes"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "net/http/pprof"

	"github.com/fiorix/go-diameter/diam"
	"github.com/fiorix/go-diameter/diam/datatype"
	"github.com/fiorix/go-diameter/diam/dict"
	"github.com/fiorix/go-diameter/diam/sm"
	rate_datatype "github.com/free5gc/RatingUtil/dataType"
	rate_dict "github.com/free5gc/RatingUtil/dict"
	"github.com/free5gc/chf/internal/logger"
	"github.com/free5gc/util/mongoapi"
	"go.mongodb.org/mongo-driver/bson"
)

const chargingDataColl = "chargingData"

func OpenServer(wg *sync.WaitGroup) {
	// Load our custom dictionary on top of the default one, which
	// always have the Base Protocol (RFC6733) and Credit Control
	// Application (RFC4006).
	err := dict.Default.Load(bytes.NewReader([]byte(rate_dict.RateDictionary)))
	if err != nil {
		logger.RatingLog.Error(err)
	}
	settings := &sm.Settings{
		OriginHost:       datatype.DiameterIdentity("server"),
		OriginRealm:      datatype.DiameterIdentity("go-diameter"),
		VendorID:         13,
		ProductName:      "go-diameter",
		FirmwareRevision: 1,
	}

	// Create the state machine (mux) and set its message handlers.
	mux := sm.New(settings)
	mux.Handle("SUR", handleSUR())
	mux.HandleFunc("ALL", handleALL) // Catch all.

	// Print error reports.
	go printErrors(mux.ErrorReports())
	go func() {
		defer func() {
			logger.FtpLog.Error("Rating Function server stopped")
			wg.Done()
		}()

		err = listen(":3868", "", "", mux)
		if err != nil {
			log.Fatal(err)
		}
	}()
}

func printErrors(ec <-chan *diam.ErrorReport) {
	for err := range ec {
		logger.RatingLog.Errorf("Diam Error Report: %v", err)
	}
}

func listen(addr, cert, key string, handler diam.Handler) error {
	// Start listening for connections.
	if len(cert) > 0 && len(key) > 0 {
		logger.RatingLog.Infof("Starting secure diameter server on", addr)
		return diam.ListenAndServeTLS(addr, cert, key, handler, nil)
	}

	logger.RatingLog.Infof("Starting diameter server on", addr)
	return diam.ListenAndServe(addr, handler, nil)
}

func buildTaffif(unitCostStr string) *rate_datatype.MonetaryTariff {
	// unitCost
	unitCost := &rate_datatype.UnitCost{}

	dotPos := strings.Index(unitCostStr, ".")
	if dotPos == -1 {
		unitCost.Exponent = datatype.Integer32(0)
		if digit, err := strconv.Atoi(unitCostStr); err == nil {
			unitCost.ValueDigits = datatype.Integer64(digit)
		}
	} else {
		if digit, err := strconv.Atoi(strings.Replace(unitCostStr, ".", "", -1)); err == nil {
			unitCost.ValueDigits = datatype.Integer64(digit)
		}
		unitCost.Exponent = datatype.Integer32(len(unitCostStr) - dotPos - 1)
	}

	return &rate_datatype.MonetaryTariff{
		CurrencyCode: datatype.Unsigned32(901),
		ScaleFactor: &rate_datatype.ScaleFactor{
			ValueDigits: datatype.Integer64(0),
			Exponent:    datatype.Integer32(0),
		},
		RateElement: &rate_datatype.RateElement{
			CCUnitType: rate_datatype.MONEY,
			UnitCost:   unitCost,
		},
	}
}

func handleSUR() diam.HandlerFunc {
	return func(c diam.Conn, m *diam.Message) {
		var sur rate_datatype.ServiceUsageRequest
		var monetaryCost datatype.Unsigned32

		var subscriberId string
		if err := m.Unmarshal(&sur); err != nil {
			logger.RatingLog.Errorf("Failed to parse message from %s: %s\n%s",
				c.RemoteAddr(), err, m)
			return
		}

		logger.RatingLog.Infof("Received SUR from %s:", c.RemoteAddr())

		sr := sur.ServiceRating
		rg := uint32(sr.ServiceIdentifier)

		switch sur.SubscriptionId.SubscriptionIdType {
		case rate_datatype.END_USER_IMSI:
			subscriberId = "imsi-" + string(sur.SubscriptionId.SubscriptionIdData)
		}
		// // Retrieve tarrif information from database

		filter := bson.M{"ueId": subscriberId, "ratingGroup": rg}
		chargingInterface, err := mongoapi.RestfulAPIGetOne(chargingDataColl, filter)
		if err != nil {
			logger.ChargingdataPostLog.Errorf("Get tarrif error: %+v", err)
		}

		unitCostStr := chargingInterface["unitCost"].(string)
		monetaryTariff := buildTaffif(unitCostStr)
		unitCost := datatype.Unsigned32(monetaryTariff.RateElement.UnitCost.ValueDigits) * datatype.Unsigned32(math.Pow10(int(monetaryTariff.RateElement.UnitCost.Exponent)))
		sua := rate_datatype.ServiceUsageResponse{
			SessionId:      sur.SessionId,
			EventTimestamp: datatype.Time(time.Now()),
			ServiceRating: &rate_datatype.ServiceRating{
				MonetaryTariff: monetaryTariff,
			},
		}

		switch sr.RequestSubType {
		// price for the consumed units
		case rate_datatype.REQ_SUBTYPE_DEBIT:
			logger.RatingLog.Warnf("Debit mode")
			monetaryCost = sr.ConsumedUnits * unitCost
			sua.ServiceRating.AllowedUnits = datatype.Unsigned32(0)
			sua.ServiceRating.Price = monetaryCost
		// price for the reserved units
		case rate_datatype.REQ_SUBTYPE_RESERVE:
			sua.ServiceRating.AllowedUnits = sr.MonetaryQuota / unitCost
			sua.ServiceRating.Price = sua.ServiceRating.AllowedUnits * unitCost
		default:
			logger.RatingLog.Warnf("Unknow request type")
			sua.ServiceRating.AllowedUnits = datatype.Unsigned32(0)
			sua.ServiceRating.Price = datatype.Unsigned32(0)
		}

		a := m.Answer(diam.Success)
		err = a.Marshal(&sua)
		if err != nil {
			logger.RatingLog.Errorf("Marshal SUA Err: %+v:", err)

		}

		_, err = a.WriteTo(c)
		if err != nil {
			logger.RatingLog.Errorf("Failed to write message to %s: %s\n%s\n",
				c.RemoteAddr(), err, a)
			return
		}
	}
}

func handleALL(c diam.Conn, m *diam.Message) {
	logger.RatingLog.Warnf("Received unexpected message from %s:\n%s", c.RemoteAddr(), m)
}