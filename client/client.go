package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-horizon/edge-utilities/logger"
	"github.com/open-horizon/edge-utilities/logger/log"
)

// SyncServiceClient is a handle for a client of the sync-service
type SyncServiceClient struct {
	serviceProtocol   string
	serviceAddress    string
	usingUnixSockets  bool
	orgID             string
	appKeySecretSet   bool
	appKey            string
	appSecret         string
	ticker            *time.Ticker
	updatesPollerStop chan bool
	inflightUpdates   map[string]bool
	pollerCount       int
	httpTransport     *http.Transport
	httpClient        http.Client
}

type syncServiceError struct {
	message string
}

func (e *syncServiceError) Error() string {
	return e.message
}

// ObjectMetaData is the metadata that identifies and defines the Sync Service object.
// Every object includes metadata (mandatory) and data (optional). The metadata and data can be updated independently.
// Each Sync Service node (ESS) has an address that is composed of the node's ID, Type, and Organization.
// To send an object to a single node set the DestType and DestID fields to match the node's Type and ID.
// To send an object to all the nodes of a certain type set DestType to the appropriate type and leave DestID empty.
// If both DestType and DestID are empty the object is sent to all nodes.
// swagger:model
type ObjectMetaData struct {
	// ObjectID is a unique identifier of the object.
	// ObjectID and ObjectType must uniquely identify the object.
	// Must be provided by the application
	ObjectID string `json:"objectID"`

	// ObjectType is the type of the object.
	// The type is used to group multiple objects, for example when checking for object updates.
	// Must be provided by the application
	ObjectType string `json:"objectType"`

	// DestID is the ID of the destination. If omitted the object is sent to all ESSs with the same DestType.
	// This field is ignored when working with ESS (the destination is the CSS).
	DestID string `json:"destinationID"`

	// DestType is the type of destination to send the object to.
	// If omitted (and if DestinationsList is omitted too) the object is broadcasted to all known destinations.
	// This field is ignored when working with ESS (the destination is always the CSS).
	DestType string `json:"destinationType"`

	// DestinationsList is the list of destinations as type:id pairs to send the object to.
	// When a DestinationsList is provided DestType and DestID must be omitted.
	// This field is ignored when working with ESS (the destination is always the CSS).
	DestinationsList []string `json:"destinationsList"`

	// Expiration is a timestamp/date indicating when the object expires.
	// When the object expires it is automatically deleted.
	// The timestamp should be provided in RFC3339 format.
	// This field is available only when working with the CSS.
	// Optional field, if omitted the object doesn't expire.
	Expiration string `json:"expiration"`

	// Version is the object's version (as used by the application).
	// Optional field, empty by default.
	Version string `json:"version"`

	// Description is a textual description of the object.
	// Optional field, empty by default.
	Description string `json:"description"`

	// Link is a link to where the data for this object can be fetched from.
	// Optional field, if omitted the data must be provided by the application.
	Link string `json:"link"`

	// Inactive is a flag indicating that this object is inactive for now.
	// An object can be created as inactive which means it is not delivered to its destination. The object can be activated later.
	// Optional field, default is false (object active).
	Inactive bool `json:"inactive"`

	// ActivationTime is a timestamp/date as to when this object should automatically be activated.
	// The timestamp should be provided in RFC3339 format.
	// Optional field, if omitted (and Inactive is true) the object is never automatically activated.
	ActivationTime string `json:"activationTime"`

	// DoNotSend is a flag indicating that this object should not be sent to any destinations.
	// Optional field, default is false (object is sent to destinations).
	DoNotSend bool `json:"doNotSend"`

	// NoData is a flag indicating that there is no data for this object.
	// Objects with no data can be used, for example, to send notifications.
	// Optional field, default is false (object includes data).
	NoData bool `json:"noData"`

	// MetaOnly is a flag that indicates that this update is only of the metadata. The current object's data is left unchanged.
	// Optional field, default is false (both data and metadata are updated).
	MetaOnly bool `json:"metaOnly"`

	// ExpectedConsumers is the number of applications that are expected to indicate that they have consumed the object.
	// Optional field, default is 1.
	// This field is used only when working with the CSS. The default value is always used on the ESS.
	ExpectedConsumers int `json:"consumers"`

	// DestinationDataURI is a URI indicating where the receiver of the object should store it.
	// Currently only file URIs are supported.
	// This field is available only when working with the CSS.
	// Optional field, if omitted the object is stored in the node's internal storage.
	DestinationDataURI string `json:"destinationDataUri"`

	// SourceDataURI is a URI indicating where the sender of the object should read the data from.
	// Currently only file URIs are supported.
	// This field is available only when working with the ESS.
	// Optional field, if omitted the object's data should be provided by the user.
	SourceDataURI string `json:"sourceDataUri"`

	// AutoDelete is a flag indicating whether to delete the object after it is delivered to all its destinations from the DestinationsList.
	// Optional field, default is false (do not delete).
	// This field is used only when working with the CSS. Objects are always deleted after delivery on the ESS.
	AutoDelete bool `json:"autodelete"`

	// OriginID is the ID of origin of the object. Set by the internal code.
	// Read only field, should not be set by users.
	OriginID string `json:"originID"`

	// OriginType is the type of origin of the object. Set by the internal code.
	// Read only field, should not be set by users.
	OriginType string `json:"originType"`

	// Deleted is a flag indicating to applications polling for updates that this object has been deleted.
	// Read only field, should not be set by users.
	Deleted bool `json:"deleted"`

	// InstanceID is an internal instance ID.
	// This field should not be set by users.
	InstanceID int64 `json:"instanceID"`
}

// Destination describes a Sync Service node.
// Each Sync Service edge node (ESS) has an address that is composed of the node's ID, Type, and Organization.
// An ESS node communicates with the CSS using either MQTT or HTTP.
type Destination struct {

	// DestOrgID is the destination organization ID
	// Each Sync Service object belongs to a single organization
	DestOrgID string `json:"destinationOrgID"`

	// DestType is the destination type
	DestType string `json:"destinationType"`

	// DestID is the destination ID
	DestID string `json:"destinationID"`

	// Communication is the communications method used by the destination to connect (can be MQTT or HTTP)
	Communication string `json:"communication"`

	// CodeVersion is the sync service code version used by the destination
	CodeVersion string `json:"codeVersion"`
}

// DestinationStatus provides information about the delivery status of an object for a certain destination.
// The status can be one of the following:
//   pending - indicates that the object is pending delivery to this destination
//   delivering - indicates that the object is being delivered to this destination
//   delivered - indicates that the object was delivered to this destination
//   consumed - indicates that the object was consumed by this destination
//   deleted - indicates that this destination acknowledged the deletion of the object
//   error - indicates that a feedback error message was received from this destination
type DestinationStatus struct {
	// DestType is the destination type
	DestType string `json:"destinationType"`

	// DestID is the destination ID
	DestID string `json:"destinationID"`

	// Status is the destination status
	Status string `json:"status"`

	// Message is the message for the destination
	Message string `json:"message"`
}

// ObjectStatus provides information about an object that is destined for a particular destination
type ObjectStatus struct {
	// OrgID is the organization ID of the object
	OrgID string `json:"orgID"`

	// ObjectType is the object type
	ObjectType string `json:"objectType"`

	// ObjectID is the object ID
	ObjectID string `json:"objectID"`

	// Status is the object status for this destination
	Status string `json:"status"`
}

const (
	// DestStatusPending indicates that the object is pending delivery to this destination
	DestStatusPending = "pending"

	// DestStatusDelivering indicates that the object is being delivered to this destination
	DestStatusDelivering = "delivering"

	// DestStatusDelivered indicates that the object was delivered to this destination
	DestStatusDelivered = "delivered"

	// DestStatusConsumed indicates that the object was consumed at this destination
	DestStatusConsumed = "consumed"

	// DestStatusDeleted indicates that this destination acknowledged the deletion of the object
	DestStatusDeleted = "deleted"

	// DestStatusError indicates that there was an error in delivering the object to this destination
	DestStatusError = "error"

	// ObjectStatusNotReady indicates that the object is not ready to be sent to destinations.
	ObjectStatusNotReady = "notReady"

	// ObjectStatusReady indicates that the object is ready to be sent to destinations.
	ObjectStatusReady = "ready"

	// ObjectStatusPartiallyReceived indicates that the object's metadata has been received but not all its data.
	ObjectStatusPartiallyReceived = "partiallyreceived"

	// ObjectStatusCompletelyReceived indicates that the full object (metadata and data) has been received.
	ObjectStatusCompletelyReceived = "completelyReceived"

	// ObjectStatusConsumed indicates that the object has been consumed by the application.
	ObjectStatusConsumed = "objconsumed"

	// ObjectStatusDeleted indicates that the object was deleted.
	ObjectStatusDeleted = "objdeleted"

	// ObjectStatusReceived indicates that the object was received by the application.
	ObjectStatusReceived = "objreceived"

	// ObjectStatusConsumedByDest indicates that the object was consumed by the other side.
	ObjectStatusConsumedByDest = "consumedByDest"
)

type objectUpdatePayload struct {
	Meta ObjectMetaData `json:"meta"`
}

type webhookUpdate struct {
	Action string `json:"action"`
	URL    string `json:"url"`
}

type bulkACLPayload struct {
	Action    string   `json:"action"`
	Usernames []string `json:"usernames"`
}

const (
	destinationsPath = "/api/v1/destinations"
	objectsPath      = "/api/v1/objects/"
	resendPath       = "/api/v1/resend"
	securityPath     = "/api/v1/security/"
)

const (
	destinationACL = "destinations"
	objectACL      = "objects"
)

// NewSyncServiceClient creates a new sync-service client instance.
// serviceProtocol defines the protocol used to connect to the Sync Service.
// It should be either "https", "http", "unix", or "secure-unix".
// If serviceProtocol is either "https" or "http", serviceAddress and servicePort specify the address and listening port of
// the Sync Service, respectively.
// If serviceProtocol is "unix" or "secure-unix", serviceAddress should contain the socket file used by the ESS, servicePort can be zero.
// Note: The serviceProtocol can be "unix" or "secure-unix", only when communicating with an ESS.
// The function returns a handle to the new client instance.
func NewSyncServiceClient(serviceProtocol string, serviceAddress string, servicePort uint16) *SyncServiceClient {
	client := SyncServiceClient{}
	client.orgID = ""
	client.updatesPollerStop = make(chan bool)
	client.httpTransport = &http.Transport{}
	client.httpClient = http.Client{Transport: client.httpTransport}
	lowercaseServiceProtocol := strings.ToLower(serviceProtocol)
	if lowercaseServiceProtocol != "unix" && lowercaseServiceProtocol != "secure-unix" {
		client.serviceProtocol = serviceProtocol
		client.serviceAddress = fmt.Sprintf("%s:%d", serviceAddress, servicePort)
	} else {
		client.usingUnixSockets = true
		if lowercaseServiceProtocol == "unix" {
			client.serviceProtocol = "http"
		} else {
			client.serviceProtocol = "https"
		}
		client.serviceAddress = "localhost:8080"
		client.httpTransport.Dial = func(proto, addr string) (conn net.Conn, err error) {
			return client.httpTransport.DialContext(context.Background(), "unix", serviceAddress)
		}
		var dialer = net.Dialer{}
		client.httpTransport.DialContext = func(ctx context.Context, proto, addr string) (conn net.Conn, err error) {
			return dialer.DialContext(ctx, "unix", serviceAddress)
		}
	}
	return &client
}

// SetOrgID sets the orgID used on client connections to the Cloud Sync Service (CSS)
// There is no need to set orgID when connecting to an Edge Sync Service (ESS)
// Each client instance can be associated with a single org.
// Applications that need to support multiple organizations should create a different client instance for each org.
func (syncClient *SyncServiceClient) SetOrgID(orgID string) {
	syncClient.orgID = orgID
}

// SetAppKeyAndSecret sets the app key and app secret to be used when communicating
// with Sync Service.
//
// The app key and app secret are used to authenticate with the Sync Service that the client is
// communicating with. The exact details of the app key and app secret depend on the Sync Service's configuration.
func (syncClient *SyncServiceClient) SetAppKeyAndSecret(key, secret string) {
	syncClient.appKey = key
	syncClient.appSecret = secret
	syncClient.appKeySecretSet = true
}

// SetCACertificate sets the CA certificate used on client secured connections if needed
// Returns nil on success or an error if any is encountered.
func (syncClient *SyncServiceClient) SetCACertificate(certPem string) error {
	certificate, err := ioutil.ReadFile(certPem)
	if err != nil {
		return err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(certificate)
	tlsConfig := &tls.Config{RootCAs: caCertPool}
	syncClient.httpTransport.TLSClientConfig = tlsConfig
	return nil
}

// StartPollingForUpdates starts the polling of the Sync Service for updates.
// Each invocation creates a Go routine that periodically polls the Sync Service for new update for a specific objectType.
// objectType specifies the type of objects the client should retrieve updates for.
// rate is the period, in seconds, between poll requests.
// updatesChannel is a channel on which the application receives the updates (as ObjectMetaData).
// Note that the updates return only the object's metadata. The object's data (if exists) can be obtained by calling FetchObjectData.
func (syncClient *SyncServiceClient) StartPollingForUpdates(objectType string, rate int, updatesChannel chan *ObjectMetaData) {
	syncClient.pollerCount++
	go syncClient.updatesPoller(objectType, rate, updatesChannel)
}

func (syncClient *SyncServiceClient) updatesPoller(objectType string, rate int, updatesChannel chan *ObjectMetaData) {
	syncClient.inflightUpdates = make(map[string]bool)
	syncClient.ticker = time.NewTicker(time.Duration(rate) * time.Second)
	firstPoll := true
	url := syncClient.createObjectURL(objectType, "", "")

	for {
		select {
		case <-syncClient.updatesPollerStop:
			return

		case <-syncClient.ticker.C:
			ok := syncClient.poll(url, firstPoll, updatesChannel)
			if ok {
				firstPoll = false
			}
		}
	}
}

// StopPollingForUpdates stops the polling of the Sync Service for updates.
func (syncClient *SyncServiceClient) StopPollingForUpdates() {
	syncClient.ticker.Stop()
	syncClient.updatesPollerStop <- true
}

func (syncClient *SyncServiceClient) poll(url string, firstPoll bool, updatesChannel chan *ObjectMetaData) bool {
	if firstPoll {
		url = url + "?received=true"
	}
	var objects []ObjectMetaData

	err := syncClient.fetchInfoHelper(url, &objects)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to poll the sync-service at %s. Error: %s\n", syncClient.serviceAddress, err)
		}
		return false
	}

	// Save the old map for the check below
	previousInflightUpdates := syncClient.inflightUpdates

	// Clean up inflight objects, remove all that aren't in the current poll, by rebuilding it.
	syncClient.inflightUpdates = make(map[string]bool)

	if objects != nil {
		for _, object := range objects {
			inflightKey := fmt.Sprintf("%s:%s:%d:%s", object.ObjectType, object.ObjectID, object.InstanceID, strconv.FormatBool(object.Deleted))
			if _, ok := previousInflightUpdates[inflightKey]; !ok {
				object := object // Make a local copy
				updatesChannel <- &object
			}
			syncClient.inflightUpdates[inflightKey] = true
		}
	}
	return true
}

// GetDestinations returns the list of registered edge nodes under an organization in the CSS.
// Returns a tuple of an array of Destination structs and an error. The error will be non-nil if an error encountered.
func (syncClient *SyncServiceClient) GetDestinations() ([]Destination, error) {
	url := fmt.Sprintf("%s://%s%s/%s", syncClient.serviceProtocol, syncClient.serviceAddress,
		destinationsPath, syncClient.orgID)
	var result []Destination

	err := syncClient.fetchInfoHelper(url, &result)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to get the destinations for %s. Error: %s\n", syncClient.orgID, err)
		}
		return nil, err
	}
	if result == nil {
		result = make([]Destination, 0)
	}

	return result, nil
}

// GetDestinationObjects returns the list of objects targeted at the specified destination
func (syncClient *SyncServiceClient) GetDestinationObjects(destType, destID string) ([]ObjectStatus, error) {
	url := fmt.Sprintf("%s://%s%s/%s/%s/%s/objects", syncClient.serviceProtocol, syncClient.serviceAddress,
		destinationsPath, syncClient.orgID, destType, destID)
	var result []ObjectStatus

	err := syncClient.fetchInfoHelper(url, &result)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to get the objects target at %s/%s/%s. Error: %s\n", syncClient.orgID, destType, destID, err)
		}
		return nil, err
	}
	if result == nil {
		result = make([]ObjectStatus, 0)
	}

	return result, nil
}

// GetObjectMetadata returns the metadata of an object.
// Returns an ObjectMetaData struct and an error. The error will be non-nil if an error encountered.
func (syncClient *SyncServiceClient) GetObjectMetadata(objectType string, objectID string) (*ObjectMetaData, error) {
	url := syncClient.createObjectURL(objectType, objectID, "")
	var result ObjectMetaData

	err := syncClient.fetchInfoHelper(url, &result)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to get the metadata for the object %s:%s. Error: %s\n", objectType, objectID, err)
		}
		return nil, err
	}

	return &result, nil
}

// GetObjectStatus returns the status of an object
// Returns a string and an error. The error will be non-nil if an error encountered.
// The string will have one of the following values:
//   notReady - The object is not ready to be sent to the destination.
//   ready - The object is ready to be sent but was not yet received by the destination.
//   received - The destination received the object's metadata but not all its data.
//   completelyReceived - The destination received the full object (metadata and data).
//   consumed - The object was consumed by the application running on the destination.
//   deleted - The object was deleted by the destination.
// Note: An empty string indicates that the object is not on the server
func (syncClient *SyncServiceClient) GetObjectStatus(objectType string, objectID string) (string, error) {
	url := syncClient.createObjectURL(objectType, objectID, "status")

	request, err := syncClient.newRequestHelper(http.MethodGet, url, nil)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to get the status for the object %s:%s. Error: %s\n", objectType, objectID, err)
		}
		return "", err
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to get the status for the object %s:%s. Error: %s\n", objectType, objectID, err)
		}
		return "", err
	}

	defer response.Body.Close()

	if response.StatusCode == 404 {
		return "", nil
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body := bodyAsString(response.Body)
		return "", &syncServiceError{body}
	}

	return bodyAsString(response.Body), nil
}

// GetObjectDestinations returns the list of destinations that an object is being sent to, along with the
// status of each "transmission"
// Returns a tuple of an array of DestinationStatus structs and an error. The error will be non-nil if an error encountered.
func (syncClient *SyncServiceClient) GetObjectDestinations(objectType string, objectID string) ([]DestinationStatus, error) {
	url := syncClient.createObjectURL(objectType, objectID, "destinations")
	var result []DestinationStatus

	err := syncClient.fetchInfoHelper(url, &result)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to get the destinations for the object %s:%s. Error: %s\n", objectType, objectID, err)
		}
		return nil, err
	}
	if result == nil {
		result = make([]DestinationStatus, 0)
	}

	return result, nil
}

// FetchObjectData fetches the data for an object given its metadata.
// object is the object's metadata that was obtained from the updatesChannel.
// write is an I/O writer to which to write the object's data.
// Returns true if the data was successfully written or false if any error was encountered.
func (syncClient *SyncServiceClient) FetchObjectData(object *ObjectMetaData, writer io.Writer) bool {
	request, err := syncClient.newRequestHelper(http.MethodGet, syncClient.createObjectURL(object.ObjectType, object.ObjectID, "data"), nil)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to get the data for the object %s:%s. Error: %s\n", object.ObjectType, object.ObjectID, err)
		}
		return false
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to get the data for the object %s:%s. Error: %s\n", object.ObjectType, object.ObjectID, err)
		}
		return false
	}

	defer response.Body.Close()

	if response.StatusCode == 404 {
		return false
	}

	if response.StatusCode >= 400 && response.StatusCode < 600 {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to get the data for the object %s:%s. Error: %s\n", object.ObjectType, object.ObjectID, bodyAsString(response.Body))
		}
		return false
	}

	_, err = io.Copy(writer, response.Body)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to read the body of a get the data for the object %s:%s. Error: %s\n", object.ObjectType, object.ObjectID, err)
		}
		return false
	}

	return true
}

// ActivateObject tells the Sync Service to mark an object as active.
// object is the metadata of the object that should be activated.
// Only objects that were created as inactive need to be activated, see ObjectMetaData.Inactive.
// Returns nil on success or an error if any is encountered.
func (syncClient *SyncServiceClient) ActivateObject(object *ObjectMetaData) error {
	body, err := syncClient.operationHelper(object, "activate")
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to activate the object %s:%s. Error: %s\n", object.ObjectType, object.ObjectID, err)
		}
	} else if len(body) != 0 {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to activate the object %s:%s. Error: %s\n",
				object.ObjectType, object.ObjectID, body)
		}
		err = &syncServiceError{body}
	}
	return err
}

// MarkObjectConsumed tells the Sync Service to mark an object consumed.
// object is the metadata of the object that should marked consumed.
// After an object is marked as consumed it will not be delivered to the application again (even if the app or the Sync Service are restarted).
// Returns nil on success or an error if any is encountered.
func (syncClient *SyncServiceClient) MarkObjectConsumed(object *ObjectMetaData) error {
	body, err := syncClient.operationHelper(object, "consumed")
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to mark the data for the object %s:%s as consumed. Error: %s\n", object.ObjectType, object.ObjectID, err)
		}
	} else if len(body) != 0 {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to mark the data for the object %s:%s as consumed. Error: %s\n",
				object.ObjectType, object.ObjectID, body)
		}
		err = &syncServiceError{body}
	}
	return err
}

// MarkObjectDeleted tells the ESS to mark an object that was deleted on the CSS as having been deleted on the ESS.
// object is the metadata of the object to be marked as deleted.
// Returns nil on success or an error if any is encountered.
func (syncClient *SyncServiceClient) MarkObjectDeleted(object *ObjectMetaData) error {
	body, err := syncClient.operationHelper(object, "deleted")
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to mark the object %s:%s as deleted. Error: %s\n", object.ObjectType, object.ObjectID, err)
		}
	} else if len(body) != 0 {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to mark the object %s:%s as deleted. Error: %s\n",
				object.ObjectType, object.ObjectID, body)
		}
		err = &syncServiceError{body}
	}
	return err
}

// MarkObjectReceived tells the Sync Service to mark an object received.
// object is the metadata of the object that should be marked received.
// After an object is marked as received it will not be delivered to the application again, unless the app restarts polling for updates.
// Returns nil on success or an error if any is encountered.
func (syncClient *SyncServiceClient) MarkObjectReceived(object *ObjectMetaData) error {
	body, err := syncClient.operationHelper(object, "received")
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to mark the data for the object %s:%s as received. Error: %s\n", object.ObjectType, object.ObjectID, err)
		}
	} else if len(body) != 0 {
		if log.IsLogging(logger.ERROR) {
			log.Error("Failed to mark the data for the object %s:%s as received. Error: %s\n",
				object.ObjectType, object.ObjectID, body)
		}
		err = &syncServiceError{body}
	}
	return err
}

func (syncClient *SyncServiceClient) webhookHelper(action string, objectType string, url string) error {
	payload := webhookUpdate{Action: action, URL: url}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	payloadBuffer := bytes.NewBuffer(payloadJSON)
	request, err := syncClient.newRequestHelper(http.MethodPut, syncClient.createObjectURL(objectType, "", ""), payloadBuffer)
	if err != nil {
		return err
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &syncServiceError{fmt.Sprintf("Failed to %s a webhook. Error: %s\n", action,
			bodyAsString(response.Body))}
	}
	return nil
}

// RegisterWebhook registers a webhook to receive updates from the Sync Service.
// objectType specifies the type of objects the client should retrieve updates for.
// url is the URL that should be called by the Sync Service when a new update is available
// Returns nil on success or an error if any was encountered
func (syncClient *SyncServiceClient) RegisterWebhook(objectType string, url string) error {
	return syncClient.webhookHelper("register", objectType, url)
}

// DeleteWebhook deletes a webhook that was previously registered with RegisterWebhook.
// objectType and url are the webhook parameters that were given to RegisterWebhook.
// Returns nil on success or an error if any was encountered
func (syncClient *SyncServiceClient) DeleteWebhook(objectType string, url string) error {
	return syncClient.webhookHelper("delete", objectType, url)
}

func (syncClient *SyncServiceClient) operationHelper(object *ObjectMetaData, operation string) (string, error) {
	request, err := syncClient.newRequestHelper(http.MethodPut, syncClient.createObjectURL(object.ObjectType, object.ObjectID, operation), nil)
	if err != nil {
		return "", err
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return bodyAsString(response.Body), nil
	}

	return "", nil
}

// UpdateObject creates/updates an object in the Sync Service
// object specifies the object's metadata
// The application must provide the ObjectID and ObjectType which uniquely identify the object.
// When creating/updating an object in the CSS the application must also provide either DestID and DestType or DestinationsList.
// All other fields in ObjectMetaData are optional and if not specified will take the default values.
// Returns nil on success or an error if any was encountered
func (syncClient *SyncServiceClient) UpdateObject(object *ObjectMetaData) error {
	payload := objectUpdatePayload{*object}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	payloadBuffer := bytes.NewBuffer(payloadJSON)
	request, err := syncClient.newRequestHelper(http.MethodPut, syncClient.createObjectURL(object.ObjectType, object.ObjectID, ""), payloadBuffer)
	if err != nil {
		return err
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &syncServiceError{fmt.Sprintf("Failed to update the object %s:%s. Error: %s\n",
			object.ObjectType, object.ObjectID, bodyAsString(response.Body))}
	}
	return nil
}

// UpdateObjectData updates the data of an object in the Sync Service
// object is the object's metadata (the one used to create the object in UpdateObject)
// reader is an I/O reader from which to read the object's data
// Note that the object's data can be updated multiple times without updating the metadata
// Returns nil on success or an error if any was encountered
func (syncClient *SyncServiceClient) UpdateObjectData(object *ObjectMetaData, reader io.Reader) error {
	request, err := syncClient.newRequestHelper(http.MethodPut, syncClient.createObjectURL(object.ObjectType, object.ObjectID, "data"), reader)
	if err != nil {
		return err
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &syncServiceError{fmt.Sprintf("Failed to update the object %s:%s. Error: %s\n",
			object.ObjectType, object.ObjectID, bodyAsString(response.Body))}
	}
	return nil
}

// DeleteObject deletes an object in the sync-service
// objectType and objectID identify the object to be deleted
// Returns nil on success or an error if any was encountered
func (syncClient *SyncServiceClient) DeleteObject(objectType string, objectID string) error {
	request, err := syncClient.newRequestHelper(http.MethodDelete, syncClient.createObjectURL(objectType, objectID, ""), nil)
	if err != nil {
		return err
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &syncServiceError{fmt.Sprintf("Failed to delete the object %s:%s. Error: %s\n",
			objectType, objectID, bodyAsString(response.Body))}
	}
	return nil
}

// Resend requests that all objects in the Sync Service be resent to an ESS.
// Used by an ESS to ask the CSS to resend it all the objects (supported only for ESS to CSS requests).
// An application only needs to use this API in case the data it previously obtained from the ESS was lost.
// Returns nil on success or an error if any was encountered
func (syncClient *SyncServiceClient) Resend() error {
	request, err := syncClient.newRequestHelper(http.MethodPut, fmt.Sprintf("%s://%s%s", syncClient.serviceProtocol, syncClient.serviceAddress, resendPath), nil)
	if err != nil {
		return err
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &syncServiceError{fmt.Sprintf("Failed to request a resend of all of the objects. Error: %s\n", bodyAsString(response.Body))}
	}
	return nil
}

// AddUsersToDestinationACL adds users to an ACL protecting a destination type.
//
// For more information on the sync service's security model see: https://github.ibm.com/edge-sync-service-dev/edge-sync-service#security
//
// Note: Adding the first user to such an ACL automatically creates it.
//
// Note: This API is for use with a CSS only.
func (syncClient *SyncServiceClient) AddUsersToDestinationACL(destType string, usernames []string) error {
	return syncClient.modifySecurityHelper(true, destinationACL, destType, usernames)
}

// RemoveUsersFromDestinationACL removes users from an ACL protecting a destination type.
//
// For more information on the sync service's security model see: https://github.ibm.com/edge-sync-service-dev/edge-sync-service#security
//
// Note: Removing the last user from such an ACL automatically deletes it.
//
// Note: This API is for use with a CSS only.
func (syncClient *SyncServiceClient) RemoveUsersFromDestinationACL(destType string, usernames []string) error {
	return syncClient.modifySecurityHelper(false, destinationACL, destType, usernames)
}

// RetrieveDestinationACL retrieves the list of users with access to a destination type protected by an ACL.
//
// For more information on the sync service's security model see: https://github.ibm.com/edge-sync-service-dev/edge-sync-service#security
//
// Returns a tuple of a slice of strings and an error. The error will be nil if the operation succeeded.
//
// Note: This API is for use with a CSS only.
func (syncClient *SyncServiceClient) RetrieveDestinationACL(destType string) ([]string, error) {
	return syncClient.retrieveACLHelper(destinationACL, destType)
}

// RetrieveAllDestinationACLs retrieves the list of destination ACLs in the organization.
//
// For more information on the sync service's security model see: https://github.ibm.com/edge-sync-service-dev/edge-sync-service#security
//
// Returns a tuple of a slice of strings and an error. The error will be nil if the operation succeeded.
//
// Note: This API is for use with a CSS only.
func (syncClient *SyncServiceClient) RetrieveAllDestinationACLs() ([]string, error) {
	return syncClient.retrieveACLHelper(destinationACL, "")
}

// AddUsersToObjectACL adds users to an ACL protecting an object type.
//
// For more information on the sync service's security model see: https://github.ibm.com/edge-sync-service-dev/edge-sync-service#security
//
// Note: Adding the first user to such an ACL automatically creates it.
func (syncClient *SyncServiceClient) AddUsersToObjectACL(objectType string, usernames []string) error {
	return syncClient.modifySecurityHelper(true, objectACL, objectType, usernames)
}

// RemoveUsersFromObjectACL removes users from an ACL protecting an object type.
//
// For more information on the sync service's security model see: https://github.ibm.com/edge-sync-service-dev/edge-sync-service#security
//
// Note: Removing the last user from such an ACL automatically deletes it.
func (syncClient *SyncServiceClient) RemoveUsersFromObjectACL(objectType string, usernames []string) error {
	return syncClient.modifySecurityHelper(false, objectACL, objectType, usernames)
}

// RetrieveObjectACL retrieves the list of users with access to an object type protected by an ACL.
//
// For more information on the sync service's security model see: https://github.ibm.com/edge-sync-service-dev/edge-sync-service#security
//
// Returns a tuple of a slice of strings and an error. The error will be nil if the operation succeeded.
func (syncClient *SyncServiceClient) RetrieveObjectACL(objectType string) ([]string, error) {
	return syncClient.retrieveACLHelper(objectACL, objectType)
}

// RetrieveAllObjectACLs retrieves the list of object ACLs in the organization.
//
// For more information on the sync service's security model see: https://github.ibm.com/edge-sync-service-dev/edge-sync-service#security
//
// Returns a tuple of a slice of strings and an error. The error will be nil if the operation succeeded.
func (syncClient *SyncServiceClient) RetrieveAllObjectACLs() ([]string, error) {
	return syncClient.retrieveACLHelper(objectACL, "")
}

func (syncClient *SyncServiceClient) modifySecurityHelper(add bool, aclType string, key string, usernames []string) error {
	action := "remove"
	messageInsert := "from"
	if add {
		action = "add"
		messageInsert = "to"
	}

	payload := bulkACLPayload{action, usernames}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	urlString := fmt.Sprintf("%s://%s%s%s/%s/%s", syncClient.serviceProtocol, syncClient.serviceAddress, securityPath, aclType, syncClient.orgID, key)
	payloadBuffer := bytes.NewBuffer(payloadJSON)
	request, err := syncClient.newRequestHelper(http.MethodPut, urlString, payloadBuffer)
	if err != nil {
		return err
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &syncServiceError{fmt.Sprintf("Failed to %s the user(s) %s the ACL for %s. Error: %s\n",
			action, messageInsert, key, bodyAsString(response.Body))}
	}
	return nil
}

func (syncClient *SyncServiceClient) retrieveACLHelper(aclType string, key string) ([]string, error) {
	urlString := fmt.Sprintf("%s://%s%s%s/%s", syncClient.serviceProtocol, syncClient.serviceAddress, securityPath, aclType, syncClient.orgID)
	if len(key) != 0 {
		urlString += "/" + key
	}
	var result []string

	err := syncClient.fetchInfoHelper(urlString, &result)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			if len(key) != 0 {
				log.Error("Failed to retrieve the %s ACL %s. Error: %s\n", aclType, key, err)
			} else {
				log.Error("Failed to retrieve all of the %s ACLs. Error: %s\n", aclType, err)
			}
		}
		return nil, err
	}
	if result == nil {
		result = make([]string, 0)
	}

	return result, nil
}

func bodyAsString(body io.Reader) string {
	var stringBody string
	bodyData, err := ioutil.ReadAll(body)
	if err != nil {
		stringBody = "Unknown"
	} else {
		stringBody = string(bodyData)
	}
	return stringBody
}

func (syncClient *SyncServiceClient) createObjectURL(objectType string, objectID string, command string) string {
	stringResult := fmt.Sprintf("%s://%s%s", syncClient.serviceProtocol, syncClient.serviceAddress, objectsPath)

	if len(syncClient.orgID) != 0 {
		stringResult = stringResult + syncClient.orgID + "/"
	}

	stringResult = stringResult + objectType

	if len(objectID) != 0 {
		stringResult = stringResult + "/" + objectID

		if len(command) != 0 {
			stringResult = stringResult + "/" + command
		}
	}

	result, err := url.Parse(stringResult)
	if err != nil {
		if log.IsLogging(logger.ERROR) {
			log.Error("An invalid URL was built %s. Error: %s.\n", stringResult, err)
		}
		return ""
	}
	return result.String()
}

func (syncClient *SyncServiceClient) fetchInfoHelper(url string, result interface{}) error {
	request, err := syncClient.newRequestHelper(http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	response, err := syncClient.httpClient.Do(request)
	if err != nil {
		return err
	}

	defer response.Body.Close()

	if response.StatusCode == 404 {
		return nil
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body := bodyAsString(response.Body)
		return &syncServiceError{body}
	}

	decoder := json.NewDecoder(response.Body)
	err = decoder.Decode(result)
	if err != nil {
		return err
	}

	return nil
}

func (syncClient *SyncServiceClient) newRequestHelper(method, url string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequest(method, url, body)
	if err == nil && syncClient.appKeySecretSet {
		request.SetBasicAuth(syncClient.appKey, syncClient.appSecret)
	}
	return request, err
}
