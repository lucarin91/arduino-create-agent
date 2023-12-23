package labsscratch

import (
	"encoding/json"

	"github.com/google/uuid"
)

type Device struct {
	PeripheralId string `json:"peripheralId"`
	Name         string `json:"name"`
	RSSI         int16  `json:"rssi"`
}

type DiscoverParams struct {
	Filters []DiscoverFilter `json:"filters"`
}

func DiscoverParamsFromJson(j json.RawMessage) (DiscoverParams, error) {
	var params DiscoverParams

	err := json.Unmarshal(j, &params)
	if err != nil {
		return DiscoverParams{}, err
	}

	return params, nil
}

type DiscoverFilter struct {
	Name       string      `json:"name"`
	NamePrefix string      `json:"namePrefix"`
	Services   []uuid.UUID `json:"services"`
}

type ConnectParams struct {
	PeripheralId string `json:"peripheralId"`
}

func ConnectParamsFromJson(j json.RawMessage) (ConnectParams, error) {
	var params ConnectParams

	err := json.Unmarshal(j, &params)
	if err != nil {
		return ConnectParams{}, err
	}

	return params, nil
}

type NotificationsParams struct {
	ServiceId        uuid.UUID `json:"serviceId"`
	CharacteristicId uuid.UUID `json:"characteristicId"`
}

func NotificationsParamsFromJson(j json.RawMessage) (NotificationsParams, error) {
	var params NotificationsParams

	err := json.Unmarshal(j, &params)
	if err != nil {
		return NotificationsParams{}, err
	}

	return params, nil
}

type UpdateParams struct {
	ServiceId        uuid.UUID `json:"serviceId"`
	CharacteristicId uuid.UUID `json:"characteristicId"`
	Message          string    `json:"message"`
	Encoding         string    `json:"encoding,omitempty"`
	WithResponse     bool      `json:"withResponse"`
}

func UpdateParamsFromJson(j json.RawMessage) (UpdateParams, error) {
	var params UpdateParams

	err := json.Unmarshal(j, &params)
	if err != nil {
		return UpdateParams{}, err
	}

	return params, nil
}

type ReadParams struct {
	ServiceId          uuid.UUID `json:"serviceId"`
	CharacteristicId   uuid.UUID `json:"characteristicId"`
	StartNotifications bool      `json:"startNotifications"`
}

func ReadParamsFromJson(j json.RawMessage) (ReadParams, error) {
	var params ReadParams

	err := json.Unmarshal(j, &params)
	if err != nil {
		return ReadParams{}, err
	}

	return params, nil
}
