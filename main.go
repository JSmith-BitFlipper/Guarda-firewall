package main

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/dgrijalva/jwt-go"
	"github.com/gorilla/mux"
	log "unknwon.dev/clog/v2"

	"github.com/JSmith-BitFlipper/webauthn-firewall-proxy/db"
	"github.com/JSmith-BitFlipper/webauthn-firewall-proxy/tool"

	"webauthn/protocol"
	"webauthn/webauthn"
	"webauthn_utils/session"
)

const (
	frontendPort     int = 8081
	backendPort      int = 3000
	reverseProxyPort int = 8081

	ENV_SESSION_KEY string = "SESSION_KEY"

	verbose bool = true
)

var (
	frontendAddress     string = fmt.Sprintf("https://localhost:%d", frontendPort)
	backendAddress      string = fmt.Sprintf("https://localhost:%d", backendPort)
	reverseProxyAddress string = fmt.Sprintf("localhost:%d", reverseProxyPort)

	webauthnAPI  *webauthn.WebAuthn
	sessionStore *session.Store
)

func logRequest(r *http.Request) {
	log.Info("%s:\t%s", r.Method, r.URL)
}

func printRequestContents(r *http.Request) error {
	// Save a copy of this request for debugging.
	requestDump, err := httputil.DumpRequest(r, true)
	if err != nil {
		return err
	}
	log.Info(string(requestDump))

	// Success!
	return nil
}

func userIDFromJWT(r *http.Request) (int64, error) {
	// The `tokenString` is the second part after the space in the `authorizationString`
	authorizationString := r.Header.Get("Authorization")
	tokenString := strings.Split(authorizationString, " ")[1]

	// Parse the JWT token
	token, _, err := new(jwt.Parser).ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		return 0, err
	}

	// Extract the `userID` from the JWT token
	userID, ok := token.Claims.(jwt.MapClaims)["id"].(float64)
	if !ok {
		return 0, fmt.Errorf("Unable to decode userID from JWT token")
	}

	// Success!
	return int64(userID), nil
}

func userIDFromSession(r *http.Request) (int64, error) {
	// Get the UserID associated with the sessionID in the cookies. This is to assure that the
	// server and the firewall are referencing the same user during the webauthn check
	url := fmt.Sprintf("%s/user/session2user", backendAddress)
	userIDReq, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}

	// Pass on the cookies from `r` to `userIDReq`, which will include the cookie with the `sessionID`
	for _, cookie := range r.Cookies() {
		userIDReq.AddCookie(cookie)
	}

	var sessionInfo struct {
		Ok     bool  `json:"ok"`
		UserID int64 `json:"uid"`
	}
	err = tool.PerformHTTP_RequestJSON(userIDReq, &sessionInfo)
	if err != nil {
		return 0, err
	}

	if !sessionInfo.Ok {
		return 0, fmt.Errorf("Unable to retrieve the userID for this cookie session")
	}

	// Success!
	return sessionInfo.UserID, nil
}

type WebauthnFirewall struct {
	*httputil.ReverseProxy
}

func NewWebauthnFirewall() *WebauthnFirewall {
	origin, _ := url.Parse(backendAddress)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	proxy.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	// Construct and return the webauthn firewall
	return &WebauthnFirewall{
		proxy,
	}
}

func (proxy *WebauthnFirewall) preamble(w http.ResponseWriter, r *http.Request) {
	if verbose {
		logRequest(r)
	}

	// Set the header info
	w.Header().Set("Access-Control-Allow-Origin", frontendAddress)
	w.Header().Set("Content-Type", "application/json")
}

func (proxy *WebauthnFirewall) proxyRequest(w http.ResponseWriter, r *http.Request) {
	// Print the HTTP request if verbosity is on
	if verbose {
		logRequest(r)
	}

	proxy.ServeHTTP(w, r)
}

func (proxy *WebauthnFirewall) optionsHandler(allowMethods ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if verbose {
			logRequest(r)
		}
		// Set the return OPTIONS
		w.Header().Set("Access-Control-Allow-Headers", "Origin,Content-Type,Accept,Authorization")
		w.Header().Set("Access-Control-Allow-Methods", strings.Join(allowMethods, ","))
		w.Header().Set("Access-Control-Allow-Origin", frontendAddress)
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		w.WriteHeader(http.StatusNoContent)
	}
}

// TODO!: Check some sort of token before responding to this since any user can
// be queried with the GET to retrieve their webauthn status
func (proxy *WebauthnFirewall) webauthnIsEnabled(w http.ResponseWriter, r *http.Request) {
	// Call the proxy preamble
	proxy.preamble(w, r)

	// Get the `user` variable passed in the url
	vars := mux.Vars(r)
	username := vars["user"]

	isEnabled := db.WebauthnStore.IsUserEnabled(db.QueryByUsername(username))

	// Marshal a response `webauthn_is_enabled` field
	json_response, err := json.Marshal(map[string]bool{"webauthn_is_enabled": isEnabled})
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return the `json_response`
	w.WriteHeader(http.StatusOK)
	w.Write(json_response)
}

func (proxy *WebauthnFirewall) beginRegister(w http.ResponseWriter, r *http.Request) {
	// Call the proxy preamble
	proxy.preamble(w, r)

	// Allow transmitting cookies, used by `sessionStore`
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	// Parse the form-data to retrieve the `http.Request` information
	username := r.FormValue("username")
	if username == "" {
		errText := "Invalid form-data parameters"
		log.Error("%v", errText)
		http.Error(w, errText, http.StatusInternalServerError)
		return
	}

	userID, err := strconv.ParseInt(r.FormValue("userID"), 10, 64)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create a new `webauthnUser` struct from the input details
	wuser := db.NewWebauthnUser(userID, username, nil)

	// TODO
	// registerOptions := func(credCreationOpts *protocol.PublicKeyCredentialCreationOptions) {
	// 	credCreationOpts.CredentialExcludeList = wuser.CredentialExcludeList()
	// }

	// generate PublicKeyCredentialCreationOptions, session data
	options, sessionData, err := webauthnAPI.BeginRegistration(
		wuser,
		// TODO registerOptions,
	)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Convert the `options` into JSON format
	json_response, err := json.Marshal(options.Response)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Save the `sessionData` as marshaled JSON
	err = sessionStore.SaveWebauthnSession("registration", sessionData, r, w)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return the `json_response`
	w.WriteHeader(http.StatusOK)
	w.Write(json_response)
}

func (proxy *WebauthnFirewall) finishRegister(w http.ResponseWriter, r *http.Request) {
	// Call the proxy preamble
	proxy.preamble(w, r)

	// Allow transmitting cookies, used by `sessionStore`
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	// Parse the form-data to retrieve the `http.Request` information
	username := r.FormValue("username")
	credentials := r.FormValue("credentials")
	if username == "" || credentials == "" {
		errText := "Invalid form-data parameters"
		log.Error("%v", errText)
		http.Error(w, errText, http.StatusInternalServerError)
		return
	}

	userID, err := strconv.ParseInt(r.FormValue("userID"), 10, 64)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create a new `webauthnUser` struct from the input details
	wuser := db.NewWebauthnUser(userID, username, nil)

	// Load the session data
	sessionData, err := sessionStore.GetWebauthnSession("registration", r)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	wcredential, err := webauthnAPI.FinishRegistration(wuser, sessionData, credentials)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Marshal a response `redirectTo` field to reload the page
	json_response, err := json.Marshal(map[string]string{"redirectTo": ""})
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Save the `wcredential` to the database
	db.WebauthnStore.Create(wuser, wcredential)

	// Success!
	w.WriteHeader(http.StatusOK)
	w.Write(json_response)
}

// TODO: They way errors are handled on the front end are slightly different
// than this `http.Error` stuff
func (proxy *WebauthnFirewall) beginAttestation_base(
	query db.WebauthnQuery, clientExtensions protocol.AuthenticationExtensions,
	w http.ResponseWriter, r *http.Request) {

	// Allow transmitting cookies, used by `sessionStore`
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	// See if the user has webauthn enabled
	isEnabled := db.WebauthnStore.IsUserEnabled(query)

	// Do nothing if the user does not have webauthn enabled
	if !isEnabled {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Get a `webauthnUser` from the input `query`
	wuser, err := db.WebauthnStore.GetWebauthnUser(query)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The `clientExtensions` in BeginLogin is now superfluous
	//
	// Generate the webauthn `options` and `sessionData`
	options, sessionData, err := webauthnAPI.BeginLogin(wuser, nil)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Add the `clientExtensions` onto the webauthn `options`
	options.Response.Extensions = clientExtensions

	// Convert the `options` into JSON format
	json_response, err := json.Marshal(options.Response)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Store session data as marshaled JSON
	err = sessionStore.SaveWebauthnSession("authentication", sessionData, r, w)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return the `json_response`
	w.WriteHeader(http.StatusOK)
	w.Write(json_response)
}

func (proxy *WebauthnFirewall) beginAttestation(w http.ResponseWriter, r *http.Request) {
	// Call the proxy preamble
	proxy.preamble(w, r)

	// Retrieve the `userID` associated with the current session
	userID, err := userIDFromSession(r)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Parse the form-data to retrieve the `http.Request` information
	authenticationText := r.FormValue("auth_text")
	if authenticationText == "" {
		errText := "Invalid form-data parameters"
		log.Error("%v", errText)
		http.Error(w, errText, http.StatusInternalServerError)
		return
	}

	// Set the transaction authentication extension
	extensions := make(protocol.AuthenticationExtensions)
	extensions["txAuthSimple"] = authenticationText

	proxy.beginAttestation_base(db.QueryByUserID(userID), extensions, w, r)
	return
}

func (proxy *WebauthnFirewall) beginLogin(w http.ResponseWriter, r *http.Request) {
	// Call the proxy preamble
	proxy.preamble(w, r)

	// Parse the form-data to retrieve the `http.Request` information
	username := r.FormValue("user_name")
	if username == "" {
		errText := "Invalid form-data parameters"
		log.Error("%v", errText)
		http.Error(w, errText, http.StatusInternalServerError)
		return
	}

	proxy.beginAttestation_base(db.QueryByUsername(username), nil, w, r)
	return
}

func (proxy *WebauthnFirewall) finishLogin(w http.ResponseWriter, r *http.Request) {
	// Print the HTTP request if verbosity is on
	if verbose {
		logRequest(r)
	}

	// Allow transmitting cookies, used by `sessionStore`
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	// Get the `data` from the `http.Request` so that it can be restored again if necessary
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Reload the `r.Body` from the `data` before reading the form fields
	r.Body = ioutil.NopCloser(bytes.NewReader(data))

	// Parse the form-data to retrieve the `http.Request` information
	username := r.FormValue("user_name")
	assertion := r.FormValue("assertion")
	if username == "" || assertion == "" {
		errText := "Invalid form-data parameters"
		log.Error("%v", errText)
		http.Error(w, errText, http.StatusInternalServerError)
		return
	}

	// See if the user has webauthn enabled
	isEnabled := db.WebauthnStore.IsUserEnabled(db.QueryByUsername(username))

	// Perform a webauthn check if webauthn is enabled for this user
	if isEnabled {
		// Get a `webauthnUser` for the requested username
		wuser, err := db.WebauthnStore.GetWebauthnUser(db.QueryByUsername(username))
		if err != nil {
			log.Error("%v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Load the session data
		sessionData, err := sessionStore.GetWebauthnSession("authentication", r)
		if err != nil {
			log.Error("%v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// There are no extensions to verify during login authentication
		var noVerify protocol.ExtensionsVerifier = func(_, _ protocol.AuthenticationExtensions) error {
			return nil
		}

		// TODO: In an actual implementation, we should perform additional checks on
		// the returned 'credential', i.e. check 'credential.Authenticator.CloneWarning'
		// and then increment the credentials counter
		_, err = webauthnAPI.FinishLogin(wuser, sessionData, noVerify, assertion)
		if err != nil {
			log.Error("%v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Reload the `r.Body` from the `data` before proxying onward
	r.Body = ioutil.NopCloser(bytes.NewReader(data))

	// Once the webauthn check passed, pass the request onward to
	// the server to check the username and password
	proxy.ServeHTTP(w, r)
	return
}

func (proxy *WebauthnFirewall) disableWebauthn(w http.ResponseWriter, r *http.Request) {
	// Call the proxy preamble
	proxy.preamble(w, r)

	// Allow transmitting cookies, used by `sessionStore`
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	// Retrieve the `userID` associated with the current session
	userID, err := userIDFromSession(r)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Parse the form-data to retrieve the `http.Request` information
	assertion := r.FormValue("assertion")
	if assertion == "" {
		errText := "Invalid form-data parameters"
		log.Error("%v", errText)
		http.Error(w, errText, http.StatusInternalServerError)
		return
	}

	// Get a `webauthnUser` for the `userID`
	wuser, err := db.WebauthnStore.GetWebauthnUser(db.QueryByUserID(userID))
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Load the session data
	sessionData, err := sessionStore.GetWebauthnSession("authentication", r)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Verify the transaction authentication text
	var verifyTxAuthSimple protocol.ExtensionsVerifier = func(_, clientDataExtensions protocol.AuthenticationExtensions) error {
		expectedExtensions := protocol.AuthenticationExtensions{
			"txAuthSimple": fmt.Sprintf("Confirm disable webauthn for %v", wuser.WebAuthnName()),
		}

		if !reflect.DeepEqual(expectedExtensions, clientDataExtensions) {
			return fmt.Errorf("Extensions verification failed: Expected %v, Received %v",
				expectedExtensions,
				clientDataExtensions)
		}

		// Success!
		return nil
	}

	// TODO: In an actual implementation, we should perform additional checks on
	// the returned 'credential', i.e. check 'credential.Authenticator.CloneWarning'
	// and then increment the credentials counter
	_, err = webauthnAPI.FinishLogin(wuser, sessionData, verifyTxAuthSimple, assertion)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Marshal a response `redirectTo` field to reload the page
	json_response, err := json.Marshal(map[string]string{"redirectTo": ""})
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Save the `credential` to the database
	db.WebauthnStore.Delete(wuser.WebAuthnName())

	// Success!
	w.WriteHeader(http.StatusOK)
	w.Write(json_response)
}

// TODO: There is a lot of opportunity to condense this code into common functions
// Can the front end just set the `username` to something garbled -> isEnabled = false, vioala!
func (proxy *WebauthnFirewall) deleteRepositoryHelper(w http.ResponseWriter, r *http.Request) {
	// Allow transmitting cookies, used by `sessionStore`
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	// Retrieve the `userID` associated with the current session
	userID, err := userIDFromSession(r)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Parse the form-data to retrieve the `http.Request` information
	assertion := r.FormValue("assertion")
	if assertion == "" {
		errText := "Invalid form-data parameters"
		log.Error("%v", errText)
		http.Error(w, errText, http.StatusInternalServerError)
		return
	}

	// Get the `username` and `reponame` variables passed in the url
	// used for the transaction string verification
	vars := mux.Vars(r)
	username := vars["username"]
	reponame := vars["reponame"]

	// See if the user has webauthn enabled
	isEnabled := db.WebauthnStore.IsUserEnabled(db.QueryByUserID(userID))

	// Perform a webauthn check if webauthn is enabled for this user
	if isEnabled {
		// Get a `webauthnUser` for the requested username
		wuser, err := db.WebauthnStore.GetWebauthnUser(db.QueryByUserID(userID))
		if err != nil {
			log.Error("%v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Load the session data
		sessionData, err := sessionStore.GetWebauthnSession("authentication", r)
		if err != nil {
			log.Error("%v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Verify the transaction authentication text
		var verifyTxAuthSimple protocol.ExtensionsVerifier = func(_, clientDataExtensions protocol.AuthenticationExtensions) error {
			expectedExtensions := protocol.AuthenticationExtensions{
				"txAuthSimple": fmt.Sprintf("Confirm repository delete: %s/%s", username, reponame),
			}

			if !reflect.DeepEqual(expectedExtensions, clientDataExtensions) {
				return fmt.Errorf("Extensions verification failed: Expected %v, Received %v",
					expectedExtensions,
					clientDataExtensions)
			}

			// Success!
			return nil
		}

		// TODO: In an actual implementation, we should perform additional checks on
		// the returned 'credential', i.e. check 'credential.Authenticator.CloneWarning'
		// and then increment the credentials counter
		_, err = webauthnAPI.FinishLogin(wuser, sessionData, verifyTxAuthSimple, assertion)
		if err != nil {
			log.Error("%v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Once the webauthn check passed, pass the request onward to
	// the server to check the username and password
	proxy.ServeHTTP(w, r)
	return
}

func (proxy *WebauthnFirewall) repoSettings(w http.ResponseWriter, r *http.Request) {
	// Print the HTTP request if verbosity is on
	if verbose {
		logRequest(r)
	}

	// Get the `data` from the `http.Request` so that it can be restored again if necessary
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Error("%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Reload the `r.Body` from the `data` before reading the form fields
	r.Body = ioutil.NopCloser(bytes.NewReader(data))

	// Parse the form-data to retrieve the `http.Request` information
	action := r.FormValue("action")
	if action == "" {
		errText := "Invalid form-data parameters"
		log.Error("%v", errText)
		http.Error(w, errText, http.StatusInternalServerError)
		return
	}

	// Reload the `r.Body` from the `data` before handling onwards
	r.Body = ioutil.NopCloser(bytes.NewReader(data))

	switch action {
	case "delete":
		// Handle deletion separately
		proxy.deleteRepositoryHelper(w, r)
	default:
		// Proxy all other requests
		proxy.ServeHTTP(w, r)
	}

	return
}

func main() {
	// Initialize a new webauthn firewall
	wfirewall := NewWebauthnFirewall()

	// Initialize the database for the firewall
	log.Info("Starting up database")
	if err := db.Init(); err != nil {
		panic("Unable to initialize database: " + err.Error())
	}

	// Register the HTTP routes
	r := mux.NewRouter()

	// Proxy routes
	r.HandleFunc("/webauthn/is_enabled/{user}", wfirewall.webauthnIsEnabled).Methods("GET")

	r.HandleFunc("/webauthn/begin_register", wfirewall.beginRegister).Methods("POST")
	r.HandleFunc("/webauthn/finish_register", wfirewall.finishRegister).Methods("POST")

	r.HandleFunc("/webauthn/begin_login", wfirewall.beginLogin).Methods("POST")
	r.HandleFunc("/user/login", wfirewall.finishLogin).Methods("POST")

	r.HandleFunc("/webauthn/begin_attestation", wfirewall.beginAttestation).Methods("POST")

	r.HandleFunc("/webauthn/disable", wfirewall.disableWebauthn).Methods("POST")
	r.HandleFunc("/{username}/{reponame}/settings", wfirewall.repoSettings).Methods("POST")

	// Catch all other requests and simply proxy them onward
	r.PathPrefix("/").HandlerFunc(wfirewall.proxyRequest).Methods("GET", "POST")

	// Start up the server
	log.Info("Starting up server on port: %d", reverseProxyPort)
	log.Info("Forwarding HTTP: %d -> %d", reverseProxyPort, backendPort)

	log.Fatal("%v", http.ListenAndServeTLS(reverseProxyAddress, "server.crt", "server.key", r))

	// Graceful stopping all loggers before exiting the program.
	log.Stop()
}

func init() {
	// Initialize the logger code
	err := log.NewConsole()
	if err != nil {
		panic("Unable to create new logger: " + err.Error())
	}

	// Initialize the Webauthn API code
	webauthnAPI, err = webauthn.New(&webauthn.Config{
		RPDisplayName: "Foobar Corp.",  // Display Name for your site
		RPID:          "localhost",     // Generally the domain name for your site
		RPOrigin:      frontendAddress, // Have the front-end be the origin URL for WebAuthn requests
	})
	if err != nil {
		panic("Unable to initialize Webauthn API: " + err.Error())
	}

	// Get the session key from the environment variable
	sessionKey, err := hex.DecodeString(os.Getenv(ENV_SESSION_KEY))
	if err != nil {
		panic("Failed to decode session key env variable: " + err.Error())
	}

	if len(sessionKey) < session.DefaultEncryptionKeyLength {
		panic(fmt.Sprintf("Session key not long enough: %d < %d",
			len(sessionKey), session.DefaultEncryptionKeyLength))
	}

	// Initialize the Webauthn `sessionStore`
	sessionStore, err = session.NewStore(sessionKey)
	if err != nil {
		panic("Failed to create webauthn session store: " + err.Error())
	}
}

func genSessionKey() {
	key, err := session.GenerateSecureKey(session.DefaultEncryptionKeyLength)
	if err != nil {
		panic("Unable to generate secure session key: " + err.Error())
	}

	fmt.Printf("export %s=%s\n", ENV_SESSION_KEY, hex.EncodeToString(key))
}
