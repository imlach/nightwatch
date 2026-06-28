package amtwsman

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/icholy/digest"

	"github.com/imlach/nightwatch/internal/bmc"
)

const (
	defaultAMTPort = "16992"

	// Reading power state: enumerate this association class, then Pull the
	// single instance, which carries the host's current PowerState.
	resourceAssocPower = "http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_AssociatedPowerManagementService"
	// Changing power state: invoke RequestPowerStateChange on this service.
	resourcePowerSvc = "http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_PowerManagementService"
	// The managed host a power change targets (ManagedElement input).
	resourceSystem = "http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ComputerSystem"

	enumNamespace     = "http://schemas.xmlsoap.org/ws/2004/09/enumeration"
	enumerateAction   = enumNamespace + "/Enumerate"
	pullAction        = enumNamespace + "/Pull"
	powerChangeAction = resourcePowerSvc + "/RequestPowerStateChange"
)

// Header SelectorSet identifying the power-management service instance. The
// values come from the firmware's own ServiceProvided reference parameters
// (CIM_PowerManagementService, hosted by SystemName "Intel(r) AMT").
const powerSvcSelectors = `
    <w:SelectorSet>
      <w:Selector Name="CreationClassName">CIM_PowerManagementService</w:Selector>
      <w:Selector Name="Name">Intel(r) AMT Power Management Service</w:Selector>
      <w:Selector Name="SystemCreationClassName">CIM_ComputerSystem</w:Selector>
      <w:Selector Name="SystemName">Intel(r) AMT</w:Selector>
    </w:SelectorSet>`

// RequestPowerStateChange input codes (DMTF requested-state enumeration).
const (
	amtPowerOn      = 2
	amtPowerCycle   = 5
	amtPowerOffHard = 6
	amtPowerOffSoft = 8
	amtPowerReset   = 10
)

type Client struct {
	Endpoint   string
	Username   string
	Password   string
	HTTPClient *http.Client
}

// Register amt with the bmc factory. redfish/idrac and ipmi will self-register
// the same way once their drivers land.
func init() {
	bmc.Register("amt", func(host, username, password string) bmc.Adapter { return New(host, username, password) })
}

func New(host, username, password string) *Client {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if username != "" {
		httpClient.Transport = &digest.Transport{
			Username: username,
			Password: password,
		}
	}
	return &Client{
		Endpoint:   normalizeEndpoint(host),
		Username:   username,
		Password:   password,
		HTTPClient: httpClient,
	}
}

// GetPowerState reads the host's current power state via a two-step WS-Man
// Enumerate then Pull of CIM_AssociatedPowerManagementService. AMT does not
// honor OptimizeEnumeration for this class, so the Pull is required.
func (c *Client) GetPowerState(ctx context.Context) bmc.Result {
	enumBody := fmt.Sprintf(`<n:Enumerate xmlns:n="%s"/>`, enumNamespace)
	enumRaw, err := c.post(ctx, enumerateAction, resourceAssocPower, c.envelope(enumerateAction, resourceAssocPower, "", enumBody))
	if err != nil {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: err.Error(), Raw: enumRaw}
	}
	enumCtx, ok := parseStringElement(enumRaw, "EnumerationContext")
	if !ok {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: "AMT enumerate returned no context", Raw: enumRaw}
	}
	pullBody := fmt.Sprintf(`<n:Pull xmlns:n="%s"><n:EnumerationContext>%s</n:EnumerationContext><n:MaxElements>1</n:MaxElements></n:Pull>`, enumNamespace, enumCtx)
	pullRaw, err := c.post(ctx, pullAction, resourceAssocPower, c.envelope(pullAction, resourceAssocPower, "", pullBody))
	if err != nil {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: err.Error(), Raw: pullRaw}
	}
	state := ParsePowerState(pullRaw)
	if state == bmc.PowerUnknown {
		return bmc.Result{OK: false, PowerState: state, Error: "power state not found in AMT response", Raw: pullRaw}
	}
	return bmc.Result{OK: true, PowerState: state, Raw: pullRaw}
}

func (c *Client) PowerOn(ctx context.Context) bmc.Result {
	return c.requestPowerChange(ctx, amtPowerOn)
}

func (c *Client) SoftOff(ctx context.Context) bmc.Result {
	return c.requestPowerChange(ctx, amtPowerOffSoft)
}

func (c *Client) HardOff(ctx context.Context) bmc.Result {
	return c.requestPowerChange(ctx, amtPowerOffHard)
}

func (c *Client) Reset(ctx context.Context) bmc.Result {
	return c.requestPowerChange(ctx, amtPowerReset)
}

func (c *Client) requestPowerChange(ctx context.Context, powerState int) bmc.Result {
	raw, err := c.post(ctx, powerChangeAction, resourcePowerSvc, c.powerChangeEnvelope(powerState))
	if err != nil {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: err.Error(), Raw: raw}
	}
	if returnValue, found := parseReturnValue(raw); found && returnValue != 0 && returnValue != 4096 {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: fmt.Sprintf("AMT power action returned %d", returnValue), Raw: raw}
	}
	return bmc.Result{OK: true, PowerState: intendedPowerState(powerState), Raw: raw}
}

func (c *Client) post(ctx context.Context, action, resource, body string) (string, error) {
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	setWSManHeaders(req, action, resource)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	return readWSManResponse(resp)
}

func readWSManResponse(resp *http.Response) (string, error) {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return string(b), fmt.Errorf("amt wsman %s", resp.Status)
	}
	return string(b), nil
}

func setWSManHeaders(req *http.Request, action, resource string) {
	req.Header.Set("Content-Type", `application/soap+xml;charset="utf-8"`)
	req.Header.Set("SOAPAction", action)
	req.Header.Set("ResourceURI", resource)
}

func normalizeEndpoint(host string) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		parsed, err := url.Parse(host)
		if err != nil || parsed.Path != "" && parsed.Path != "/" {
			return host
		}
		parsed.Path = "/wsman"
		return parsed.String()
	}
	if strings.Contains(host, ":") {
		return "http://" + host + "/wsman"
	}
	return "http://" + host + ":" + defaultAMTPort + "/wsman"
}

func (c *Client) powerChangeEnvelope(powerState int) string {
	body := fmt.Sprintf(`<p:RequestPowerStateChange_INPUT xmlns:p="%s"><p:PowerState>%d</p:PowerState><p:ManagedElement><a:Address>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:Address><a:ReferenceParameters><w:ResourceURI>%s</w:ResourceURI><w:SelectorSet><w:Selector Name="CreationClassName">CIM_ComputerSystem</w:Selector><w:Selector Name="Name">ManagedSystem</w:Selector></w:SelectorSet></a:ReferenceParameters></p:ManagedElement></p:RequestPowerStateChange_INPUT>`, resourcePowerSvc, powerState, resourceSystem)
	return c.envelope(powerChangeAction, resourcePowerSvc, powerSvcSelectors, body)
}

func (c *Client) envelope(action, resource, selectorSet, body string) string {
	messageID := "uuid:" + randomHex(16)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:w="http://schemas.dmtf.org/wbem/wsman/1/wsman.xsd">
  <s:Header>
    <a:To>%s</a:To>
    <w:ResourceURI s:mustUnderstand="true">%s</w:ResourceURI>
    <a:ReplyTo><a:Address s:mustUnderstand="true">http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:Address></a:ReplyTo>
    <a:Action s:mustUnderstand="true">%s</a:Action>
    <w:MaxEnvelopeSize s:mustUnderstand="true">153600</w:MaxEnvelopeSize>
    <a:MessageID>%s</a:MessageID>
    <w:OperationTimeout>PT60S</w:OperationTimeout>%s
  </s:Header>
  <s:Body>%s</s:Body>
</s:Envelope>`, c.Endpoint, resource, action, messageID, selectorSet, body)
}

// ParsePowerState extracts the PowerState property from a Pull/Get response.
func ParsePowerState(raw string) bmc.PowerState {
	value, found := parseIntegerElement(raw, "PowerState")
	if !found {
		return bmc.PowerUnknown
	}
	return mapReadPowerState(value)
}

func parseReturnValue(raw string) (int, bool) {
	return parseIntegerElement(raw, "ReturnValue")
}

func parseIntegerElement(raw, name string) (int, bool) {
	text, ok := parseStringElement(raw, name)
	if !ok {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, false
	}
	return code, true
}

func parseStringElement(raw, name string) (string, bool) {
	decoder := xml.NewDecoder(strings.NewReader(raw))
	for {
		tok, err := decoder.Token()
		if err != nil {
			return "", false
		}
		start, ok := tok.(xml.StartElement)
		if !ok || !strings.EqualFold(start.Name.Local, name) {
			continue
		}
		var value string
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return "", false
		}
		return strings.TrimSpace(value), true
	}
}

// mapReadPowerState maps a CIM_AssociatedPowerManagementService.PowerState
// reading to on/off. Sleep/hibernate and transient states report unknown.
func mapReadPowerState(code int) bmc.PowerState {
	switch code {
	case 2: // On
		return bmc.PowerOn
	case 6, 8, 13: // Off-Hard, Off-Soft, Off-Hard Graceful
		return bmc.PowerOff
	default:
		return bmc.PowerUnknown
	}
}

// intendedPowerState maps a RequestPowerStateChange input code to the power
// state the host is heading toward, for reporting a power action's intent.
func intendedPowerState(code int) bmc.PowerState {
	switch code {
	case amtPowerOn, amtPowerCycle, amtPowerReset:
		return bmc.PowerOn
	case amtPowerOffHard, amtPowerOffSoft:
		return bmc.PowerOff
	default:
		return bmc.PowerUnknown
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
