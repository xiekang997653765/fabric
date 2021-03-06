/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// EXPERIMENTAL - Package config allows the creation, retrieval, and modification of channel configtx.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
	cb "github.com/hyperledger/fabric-protos-go/common"
	mb "github.com/hyperledger/fabric-protos-go/msp"
)

// Channel is a channel configuration.
type Channel struct {
	Consortium   string
	Application  Application
	Orderer      Orderer
	Consortiums  []Consortium
	Capabilities []string
	Policies     map[string]Policy
}

// Policy is an expression used to define rules for access to channels, chaincodes, etc.
type Policy struct {
	Type string
	Rule string
}

// Organization is an organization in the channel configuration.
type Organization struct {
	Name     string
	Policies map[string]Policy
	MSP      MSP

	// AnchorPeers contains the endpoints of anchor peers for each application organization.
	AnchorPeers      []Address
	OrdererEndpoints []string
}

// Address contains the hostname and port for an endpoint.
type Address struct {
	Host string
	Port int
}

type standardConfigValue struct {
	key   string
	value proto.Message
}

type standardConfigPolicy struct {
	key   string
	value *cb.Policy
}

// ConfigTx wraps a config transaction
type ConfigTx struct {
	// original state of the config
	base *cb.Config
	// modified state of the config
	updated *cb.Config
}

// New returns an config.
func New(config *cb.Config) ConfigTx {
	return ConfigTx{
		base: config,
		// Clone the base config for processing updates
		updated: proto.Clone(config).(*cb.Config),
	}
}

// Base returns the base config
func (c *ConfigTx) Base() *cb.Config {
	return c.base
}

// Updated returns the updated config
func (c *ConfigTx) Updated() *cb.Config {
	return c.updated
}

// ComputeUpdate computes the ConfigUpdate from a base and modified config transaction.
func (c *ConfigTx) ComputeUpdate(channelID string) (*cb.ConfigUpdate, error) {
	if channelID == "" {
		return nil, errors.New("channel ID is required")
	}

	updt, err := computeConfigUpdate(c.base, c.updated)
	if err != nil {
		return nil, fmt.Errorf("failed to compute update: %v", err)
	}

	updt.ChannelId = channelID

	return updt, nil
}

// ChannelConfiguration returns a channel configuration value from a config transaction.
func (c *ConfigTx) ChannelConfiguration() (Channel, error) {
	channelGroup := c.base.ChannelGroup
	var (
		err          error
		consortium   string
		application  Application
		orderer      Orderer
		consortiums  []Consortium
		capabilities []string
	)

	if _, ok := channelGroup.Values[ConsortiumKey]; ok {
		consortiumProto := &cb.Consortium{}
		err := unmarshalConfigValueAtKey(channelGroup, ConsortiumKey, consortiumProto)
		if err != nil {
			return Channel{}, err
		}
		consortium = consortiumProto.Name
	}

	if _, ok := channelGroup.Groups[ApplicationGroupKey]; ok {
		application, err = c.ApplicationConfiguration()
		if err != nil {
			return Channel{}, err
		}
	}

	if _, ok := channelGroup.Groups[OrdererGroupKey]; ok {
		orderer, err = c.OrdererConfiguration()
		if err != nil {
			return Channel{}, err
		}
	}

	if _, ok := channelGroup.Groups[ConsortiumsGroupKey]; ok {
		consortiums, err = c.Consortiums()
		if err != nil {
			return Channel{}, err
		}
	}

	if _, ok := channelGroup.Values[CapabilitiesKey]; ok {
		capabilities, err = c.ChannelCapabilities()
		if err != nil {
			return Channel{}, err
		}
	}

	policies, err := c.ChannelPolicies()
	if err != nil {
		return Channel{}, err
	}

	return Channel{
		Consortium:   consortium,
		Application:  application,
		Orderer:      orderer,
		Consortiums:  consortiums,
		Capabilities: capabilities,
		Policies:     policies,
	}, nil
}

// NewCreateChannelTx creates a create channel tx using the provided application channel
// configuration and returns an unsigned envelope for an application channel creation transaction.
func NewCreateChannelTx(channelConfig Channel, channelID string) (*cb.Envelope, error) {
	var err error

	if channelID == "" {
		return nil, errors.New("profile's channel ID is required")
	}

	ct, err := defaultConfigTemplate(channelConfig)
	if err != nil {
		return nil, fmt.Errorf("creating default config template: %v", err)
	}

	newChannelConfigUpdate, err := newChannelCreateConfigUpdate(channelID, channelConfig, ct)
	if err != nil {
		return nil, fmt.Errorf("creating channel create config update: %v", err)
	}

	configUpdate, err := proto.Marshal(newChannelConfigUpdate)
	if err != nil {
		return nil, fmt.Errorf("marshaling new channel config update: %v", err)
	}

	newConfigUpdateEnv := &cb.ConfigUpdateEnvelope{
		ConfigUpdate: configUpdate,
	}

	env, err := newEnvelope(cb.HeaderType_CONFIG_UPDATE, channelID, newConfigUpdateEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create envelope: %v", err)
	}

	return env, nil
}

// newChannelGroup defines the root of the channel configuration.
func newChannelGroup(channelConfig Channel) (*cb.ConfigGroup, error) {
	var err error

	channelGroup := newConfigGroup()

	if channelConfig.Consortium != "" {
		err = setValue(channelGroup, consortiumValue(channelConfig.Consortium), "")
		if err != nil {
			return nil, err
		}
	}

	channelGroup.Groups[ApplicationGroupKey], err = newApplicationGroup(channelConfig.Application)
	if err != nil {
		return nil, fmt.Errorf("failed to create application group: %v", err)
	}

	return channelGroup, nil
}

// newSystemChannelGroup defines the root of the system channel configuration.
func newSystemChannelGroup(channelConfig Channel) (*cb.ConfigGroup, error) {
	var err error

	channelGroup := newConfigGroup()

	err = addPolicies(channelGroup, channelConfig.Policies, AdminsPolicyKey)
	if err != nil {
		return nil, fmt.Errorf("failed to add system channel policies: %v", err)
	}

	if len(channelConfig.Orderer.Addresses) == 0 {
		return nil, errors.New("orderer endpoints is not defined in channel config")
	}

	err = setValue(channelGroup, ordererAddressesValue(channelConfig.Orderer.Addresses), ordererAdminsPolicyName)
	if err != nil {
		return nil, err
	}

	if channelConfig.Consortium == "" {
		return nil, errors.New("consortium is not defined in channel config")
	}

	err = setValue(channelGroup, consortiumValue(channelConfig.Consortium), AdminsPolicyKey)
	if err != nil {
		return nil, err
	}

	if len(channelConfig.Capabilities) == 0 {
		return nil, errors.New("capabilities is not defined in channel config")
	}

	err = setValue(channelGroup, capabilitiesValue(channelConfig.Capabilities), AdminsPolicyKey)
	if err != nil {
		return nil, err
	}

	ordererGroup, err := newOrdererGroup(channelConfig.Orderer)
	if err != nil {
		return nil, err
	}
	channelGroup.Groups[OrdererGroupKey] = ordererGroup

	consortiumsGroup, err := newConsortiumsGroup(channelConfig.Consortiums)
	if err != nil {
		return nil, err
	}
	channelGroup.Groups[ConsortiumsGroupKey] = consortiumsGroup

	return channelGroup, nil
}

// setValue sets the value as ConfigValue in the ConfigGroup.
func setValue(cg *cb.ConfigGroup, value *standardConfigValue, modPolicy string) error {
	v, err := proto.Marshal(value.value)
	if err != nil {
		return fmt.Errorf("marshaling standard config value '%s': %v", value.key, err)
	}

	cg.Values[value.key] = &cb.ConfigValue{
		Value:     v,
		ModPolicy: modPolicy,
	}

	return nil
}

// implicitMetaFromString parses a *cb.ImplicitMetaPolicy from an input string.
func implicitMetaFromString(input string) (*cb.ImplicitMetaPolicy, error) {
	args := strings.Split(input, " ")
	if len(args) != 2 {
		return nil, fmt.Errorf("expected two space separated tokens, but got %d", len(args))
	}

	res := &cb.ImplicitMetaPolicy{
		SubPolicy: args[1],
	}

	switch args[0] {
	case cb.ImplicitMetaPolicy_ANY.String():
		res.Rule = cb.ImplicitMetaPolicy_ANY
	case cb.ImplicitMetaPolicy_ALL.String():
		res.Rule = cb.ImplicitMetaPolicy_ALL
	case cb.ImplicitMetaPolicy_MAJORITY.String():
		res.Rule = cb.ImplicitMetaPolicy_MAJORITY
	default:
		return nil, fmt.Errorf("unknown rule type '%s', expected ALL, ANY, or MAJORITY", args[0])
	}

	return res, nil
}

// ordererAddressesValue returns the a config definition for the orderer addresses.
// It is a value for the /Channel group.
func ordererAddressesValue(addresses []Address) *standardConfigValue {
	var addrs []string
	for _, a := range addresses {
		addrs = append(addrs, fmt.Sprintf("%s:%d", a.Host, a.Port))
	}

	return &standardConfigValue{
		key: OrdererAddressesKey,
		value: &cb.OrdererAddresses{
			Addresses: addrs,
		},
	}
}

// mspValue returns the config definition for an MSP.
// It is a value for the /Channel/Orderer/*, /Channel/Application/*, and /Channel/Consortiums/*/*/* groups.
func mspValue(mspDef *mb.MSPConfig) *standardConfigValue {
	return &standardConfigValue{
		key:   MSPKey,
		value: mspDef,
	}
}

// defaultConfigTemplate generates a config template based on the assumption that
// the input profile is a channel creation template and no system channel context
// is available.
func defaultConfigTemplate(channelConfig Channel) (*cb.ConfigGroup, error) {
	channelGroup, err := newChannelGroup(channelConfig)
	if err != nil {
		return nil, err
	}

	if _, ok := channelGroup.Groups[ApplicationGroupKey]; !ok {
		return nil, errors.New("channel template config must contain an application section")
	}

	channelGroup.Groups[ApplicationGroupKey].Values = nil
	channelGroup.Groups[ApplicationGroupKey].Policies = nil

	return channelGroup, nil
}

// newChannelCreateConfigUpdate generates a ConfigUpdate which can be sent to the orderer to create a new channel.
// Optionally, the channel group of the ordering system channel may be passed in, and the resulting ConfigUpdate
// will extract the appropriate versions from this file.
func newChannelCreateConfigUpdate(channelID string, channelConfig Channel, templateConfig *cb.ConfigGroup) (*cb.ConfigUpdate, error) {
	newChannelGroup, err := newChannelGroup(channelConfig)
	if err != nil {
		return nil, err
	}

	updt, err := computeConfigUpdate(&cb.Config{ChannelGroup: templateConfig}, &cb.Config{ChannelGroup: newChannelGroup})
	if err != nil {
		return nil, fmt.Errorf("computing update: %v", err)
	}

	wsValue, err := proto.Marshal(&cb.Consortium{
		Name: channelConfig.Consortium,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling consortium: %v", err)
	}

	// Add the consortium name to create the channel for into the write set as required
	updt.ChannelId = channelID
	updt.ReadSet.Values[ConsortiumKey] = &cb.ConfigValue{Version: 0}
	updt.WriteSet.Values[ConsortiumKey] = &cb.ConfigValue{
		Version: 0,
		Value:   wsValue,
	}

	return updt, nil
}

// newConfigGroup creates an empty *cb.ConfigGroup.
func newConfigGroup() *cb.ConfigGroup {
	return &cb.ConfigGroup{
		Groups:   make(map[string]*cb.ConfigGroup),
		Values:   make(map[string]*cb.ConfigValue),
		Policies: make(map[string]*cb.ConfigPolicy),
	}
}

// newEnvelope creates an unsigned envelope of type txType using with the marshalled
// cb.ConfigGroupEnvelope proto message.
func newEnvelope(
	txType cb.HeaderType,
	channelID string,
	dataMsg proto.Message,
) (*cb.Envelope, error) {
	payloadChannelHeader := channelHeader(txType, msgVersion, channelID, epoch)
	payloadSignatureHeader := &cb.SignatureHeader{}

	data, err := proto.Marshal(dataMsg)
	if err != nil {
		return nil, fmt.Errorf("marshaling envelope data: %v", err)
	}

	payloadHeader, err := payloadHeader(payloadChannelHeader, payloadSignatureHeader)
	if err != nil {
		return nil, fmt.Errorf("making payload header: %v", err)
	}

	paylBytes, err := proto.Marshal(
		&cb.Payload{
			Header: payloadHeader,
			Data:   data,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("marshaling payload: %v", err)
	}

	env := &cb.Envelope{
		Payload: paylBytes,
	}

	return env, nil
}

// channelHeader creates a ChannelHeader.
func channelHeader(headerType cb.HeaderType, version int32, channelID string, epoch uint64) *cb.ChannelHeader {
	return &cb.ChannelHeader{
		Type:    int32(headerType),
		Version: version,
		Timestamp: &timestamp.Timestamp{
			Seconds: ptypes.TimestampNow().GetSeconds(),
		},
		ChannelId: channelID,
		Epoch:     epoch,
	}
}

// payloadHeader creates a Payload Header.
func payloadHeader(ch *cb.ChannelHeader, sh *cb.SignatureHeader) (*cb.Header, error) {
	channelHeader, err := proto.Marshal(ch)
	if err != nil {
		return nil, fmt.Errorf("marshaling channel header: %v", err)
	}

	signatureHeader, err := proto.Marshal(sh)
	if err != nil {
		return nil, fmt.Errorf("marshaling signature header: %v", err)
	}

	return &cb.Header{
		ChannelHeader:   channelHeader,
		SignatureHeader: signatureHeader,
	}, nil
}

// concatenateBytes combines multiple arrays of bytes, for signatures or digests
// over multiple fields.
func concatenateBytes(data ...[]byte) []byte {
	res := []byte{}
	for i := range data {
		res = append(res, data[i]...)
	}

	return res
}

// unmarshalConfigValueAtKey unmarshals the value for the specified key in a config group
// into the designated proto message.
func unmarshalConfigValueAtKey(group *cb.ConfigGroup, key string, msg proto.Message) error {
	valueAtKey, ok := group.Values[key]
	if !ok {
		return fmt.Errorf("config does not contain value for %s", key)
	}

	err := proto.Unmarshal(valueAtKey.Value, msg)
	if err != nil {
		return fmt.Errorf("unmarshalling %s: %v", key, err)
	}

	return nil
}

func parseAddress(address string) (string, int, error) {
	hostport := strings.Split(address, ":")
	if len(hostport) != 2 {
		return "", 0, fmt.Errorf("unable to parse host and port from %s", address)
	}

	host := hostport[0]
	port := hostport[1]

	portNum, err := strconv.Atoi(port)
	if err != nil {
		return "", 0, err
	}

	return host, portNum, nil
}
