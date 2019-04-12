package cmd

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jenkins-x/jx/pkg/gits"
	"github.com/jenkins-x/jx/pkg/jenkinsfile"
	"github.com/jenkins-x/jx/pkg/jx/cmd/opts"
	"github.com/jenkins-x/jx/pkg/kube/services"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"
	"github.com/sirupsen/logrus"

	"github.com/pkg/errors"

	"github.com/jenkins-x/jx/pkg/jx/cmd/templates"
	"github.com/spf13/cobra"
	pipelineapi "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	environmentControllerService       = "environment-controller"
	environmentControllerHmacSecret    = "environment-controller-hmac"
	environmentControllerHmacSecretKey = "hmac"
)

// ControllerEnvironmentOptions holds the command line arguments
type ControllerEnvironmentOptions struct {
	*opts.CommonOptions
	BindAddress           string
	Path                  string
	Port                  int
	NoGitCredeentialsInit bool
	NoRegisterWebHook     bool
	GitServerURL          string
	GitOwner              string
	GitRepo               string
	SourceURL             string
	WebHookURL            string
	Branch                string
	Labels                map[string]string

	secret []byte
}

var (
	controllerEnvironmentsLong = templates.LongDesc(`A controller which takes a webhook and updates the environment via GitOps for remote clusters`)

	controllerEnvironmentsExample = templates.Examples(`
			# run the environment controller
			jx controller environment
		`)
)

// NewCmdControllerEnvironment creates the command
func NewCmdControllerEnvironment(commonOpts *opts.CommonOptions) *cobra.Command {
	options := ControllerEnvironmentOptions{
		CommonOptions: commonOpts,
	}
	cmd := &cobra.Command{
		Use:     "environment",
		Short:   "A controller which takes a webhook and updates the environment via GitOps for remote clusters",
		Long:    controllerEnvironmentsLong,
		Example: controllerEnvironmentsExample,
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			CheckErr(err)
		},
	}

	cmd.Flags().IntVarP(&options.Port, optionPort, "", 8080, "The TCP port to listen on.")
	cmd.Flags().StringVarP(&options.BindAddress, optionBind, "", "",
		"The interface address to bind to (by default, will listen on all interfaces/addresses).")
	cmd.Flags().StringVarP(&options.Path, "path", "p", "/",
		"The path to listen on for requests to trigger a pipeline run.")
	cmd.Flags().StringVarP(&options.ServiceAccount, "service-account", "", "tekton-bot", "The Kubernetes ServiceAccount to use to run the pipeline")
	cmd.Flags().BoolVarP(&options.NoGitCredeentialsInit, "no-git-init", "", false, "Disables checking we have setup git credentials on startup")
	cmd.Flags().BoolVarP(&options.NoGitCredeentialsInit, "no-register-webhook", "", false, "Disables checking to register the webhook on startup")
	cmd.Flags().StringVarP(&options.SourceURL, "source-url", "s", "", "The source URL of the environment git repository")
	cmd.Flags().StringVarP(&options.GitServerURL, "git-server-url", "", "", "The git server URL. If not specified defaults to $GIT_SERVER_URL")
	cmd.Flags().StringVarP(&options.GitOwner, "owner", "o", "", "The git repository owner. If not specified defaults to $OWNER")
	cmd.Flags().StringVarP(&options.GitRepo, "repo", "r", "", "The git repository name. If not specified defaults to $REPO")
	cmd.Flags().StringVarP(&options.WebHookURL, "webhook-url", "w", "", "The external WebHook URL of this controller to register with the git provider. If not specified defaults to $WEBHOOK_URL")
	return cmd
}

// Run will implement this command
func (o *ControllerEnvironmentOptions) Run() error {
	var err error
	if o.SourceURL != "" {
		gitInfo, err := gits.ParseGitURL(o.SourceURL)
		if err != nil {
			return err
		}
		if o.GitServerURL == "" {
			o.GitServerURL = gitInfo.ProviderURL()
		}
		if o.GitOwner == "" {
			o.GitOwner = gitInfo.Organisation
		}
		if o.GitRepo == "" {
			o.GitRepo = gitInfo.Name
		}
	}
	if o.GitServerURL == "" {
		o.GitServerURL = os.Getenv("GIT_SERVER_URL")
		if o.GitServerURL == "" {
			return util.MissingOption("git-server-url")
		}
	}
	if o.GitOwner == "" {
		o.GitOwner = os.Getenv("OWNER")
		if o.GitOwner == "" {
			return util.MissingOption("owner")
		}
	}
	if o.GitRepo == "" {
		o.GitRepo = os.Getenv("REPO")
		if o.GitRepo == "" {
			return util.MissingOption("repo")
		}
	}

	if o.Branch == "" {
		o.Branch = os.Getenv("BRANCH")
		if o.Branch == "" {
			o.Branch = "master"
		}
	}
	if o.WebHookURL == "" {
		o.WebHookURL = os.Getenv("WEBHOOK_URL")
		if o.WebHookURL == "" {
			o.WebHookURL, err = o.discoverWebHookURL()
			if err != nil {
				return err
			}
		}
	}
	if o.SourceURL == "" {
		o.SourceURL = util.UrlJoin(o.GitServerURL, o.GitOwner, o.GitRepo)
	}
	log.Infof("using environment source directory %s and external webhook URL: %s\n", util.ColorInfo(o.SourceURL), util.ColorInfo(o.WebHookURL))
	o.secret, err = o.loadOrCreateHmacSecret()
	if err != nil {
		return errors.Wrapf(err, "loading hmac secret")
	}

	if !o.NoGitCredeentialsInit {
		err = o.InitGitConfigAndUser()
		if err != nil {
			return err
		}
	}

	if !o.NoRegisterWebHook {
		err = o.registerWebHook(o.WebHookURL, o.secret)
		if err != nil {
			return err
		}
	}

	mux := http.NewServeMux()
	mux.Handle(o.Path, http.HandlerFunc(o.handleRequests))
	mux.Handle(HealthPath, http.HandlerFunc(o.health))
	mux.Handle(ReadyPath, http.HandlerFunc(o.ready))

	log.Infof("Environment Controller is now listening on %s for WebHooks from the source repository %s to trigger promotions\n", util.ColorInfo(util.UrlJoin(o.WebHookURL, o.Path)), util.ColorInfo(o.SourceURL))
	return http.ListenAndServe(":"+strconv.Itoa(o.Port), mux)
}

// health returns either HTTP 204 if the service is healthy, otherwise nothing ('cos it's dead).
func (o *ControllerEnvironmentOptions) health(w http.ResponseWriter, r *http.Request) {
	log.Debug("Health check")
	w.WriteHeader(http.StatusNoContent)
}

// ready returns either HTTP 204 if the service is ready to serve requests, otherwise HTTP 503.
func (o *ControllerEnvironmentOptions) ready(w http.ResponseWriter, r *http.Request) {
	log.Debug("Ready check")
	if o.isReady() {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

// handle request for pipeline runs
func (o *ControllerEnvironmentOptions) startPipelineRun(w http.ResponseWriter, r *http.Request) {
	err := o.stepGitCredentials()
	if err != nil {
		log.Warn(err.Error())
	}

	sourceURL := o.SourceURL
	branch := o.Branch
	revision := "master"
	pr := &StepCreateTaskOptions{}
	pr.PipelineKind = jenkinsfile.PipelineKindRelease

	copy := *o.CommonOptions
	pr.CommonOptions = &copy

	// defaults
	pr.SourceName = "source"
	pr.Duration = time.Second * 20
	pr.Trigger = string(pipelineapi.PipelineTriggerTypeManual)
	pr.CloneGitURL = sourceURL
	pr.DeleteTempDir = true
	pr.Branch = branch
	pr.Revision = revision
	pr.ServiceAccount = o.ServiceAccount

	// turn map into string array with = separator to match type of custom labels which are CLI flags
	for key, value := range o.Labels {
		pr.CustomLabels = append(pr.CustomLabels, fmt.Sprintf("%s=%s", key, value))
	}

	log.Infof("triggering pipeline for repo %s branch %s revision %s\n", sourceURL, branch, revision)

	err = pr.Run()
	if err != nil {
		o.returnError(err, err.Error(), w, r)
		return
	}

	results := &PipelineRunResponse{
		Resources: pr.Results.ObjectReferences(),
	}
	err = o.marshalPayload(w, r, results)
	if err != nil {
		o.returnError(err, "failed to marshal payload", w, r)
	}
	return
}

// discoverWebHookURL lets try discover the webhook URL from the Service
func (o *ControllerEnvironmentOptions) discoverWebHookURL() (string, error) {
	kubeCtl, ns, err := o.KubeClientAndNamespace()
	if err != nil {
		return "", err
	}
	serviceInterface := kubeCtl.CoreV1().Services(ns)
	svc, err := serviceInterface.Get(environmentControllerService, metav1.GetOptions{})
	if err != nil {
		return "", errors.Wrapf(err, "failed to find Service %s in namespace %s", environmentControllerService, ns)
	}
	u := services.GetServiceURL(svc)
	if u != "" {
		return u, nil
	}
	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		// lets wait for the LoadBalancer to be resolved
		loggedWait := false
		fn := func() (bool, error) {
			svc, err := serviceInterface.Get(environmentControllerService, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			u = services.GetServiceURL(svc)
			if u != "" {
				return true, nil
			}

			if !loggedWait {
				loggedWait = true
				log.Infof("waiting for the external IP on the service %s in namespace %s ...\n", environmentControllerService, ns)
			}
			return false, nil
		}
		err = o.RetryUntilTrueOrTimeout(time.Minute*5, time.Second*3, fn)
		if u != "" {
			return u, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find external URL of Service %s in namespace %s", environmentControllerService, ns)
}

// loadOrCreateHmacSecret loads the hmac secret
func (o *ControllerEnvironmentOptions) loadOrCreateHmacSecret() ([]byte, error) {
	kubeCtl, ns, err := o.KubeClientAndNamespace()
	if err != nil {
		return nil, err
	}
	secretInterface := kubeCtl.CoreV1().Secrets(ns)
	secret, err := secretInterface.Get(environmentControllerHmacSecret, metav1.GetOptions{})
	if err == nil {
		if secret.Data == nil || len(secret.Data[environmentControllerHmacSecretKey]) == 0 {
			// lets update the secret with a valid hmac token
			err = o.ensureHmacTokenPopulated()
			if err != nil {
				return nil, err
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data[environmentControllerHmacSecretKey] = []byte(o.HMACToken)
			secret, err = secretInterface.Update(secret)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to update HMAC token secret %s in namespace %s", environmentControllerHmacSecret, ns)
			}
		}
	} else {
		err = o.ensureHmacTokenPopulated()
		if err != nil {
			return nil, err
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: environmentControllerHmacSecret,
			},
			Data: map[string][]byte{
				environmentControllerHmacSecretKey: []byte(o.HMACToken),
			},
		}
		secret, err = secretInterface.Create(secret)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create HMAC token secret %s in namespace %s", environmentControllerHmacSecret, ns)
		}
	}
	if secret == nil || secret.Data == nil {
		return nil, fmt.Errorf("no Secret %s found in namespace %s", environmentControllerHmacSecret, ns)
	}
	return secret.Data[environmentControllerHmacSecretKey], nil
}

func (o *ControllerEnvironmentOptions) ensureHmacTokenPopulated() error {
	if o.HMACToken == "" {
		var err error
		// why 41?  seems all examples so far have a random token of 41 chars
		o.HMACToken, err = util.RandStringBytesMaskImprSrc(41)
		if err != nil {
			return errors.Wrapf(err, "failed to generate hmac token")
		}
	}
	return nil
}

func (o *ControllerEnvironmentOptions) isReady() bool {
	// TODO a better readiness check
	return true
}

func (o *ControllerEnvironmentOptions) unmarshalBody(w http.ResponseWriter, r *http.Request, result interface{}) error {
	// TODO assume JSON for now
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return errors.Wrap(err, "reading the JSON request body")
	}
	err = json.Unmarshal(data, result)
	if err != nil {
		return errors.Wrap(err, "unmarshalling the JSON request body")
	}
	return nil
}

func (o *ControllerEnvironmentOptions) marshalPayload(w http.ResponseWriter, r *http.Request, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrapf(err, "marshalling the JSON payload %#v", payload)
	}
	w.Write(data)
	return nil
}

func (o *ControllerEnvironmentOptions) onError(err error) {
	if err != nil {
		log.Errorf("%v", err)
	}
}

func (o *ControllerEnvironmentOptions) returnError(err error, message string, w http.ResponseWriter, r *http.Request) {
	log.Errorf("%v %s", err, message)

	o.onError(err)
	w.WriteHeader(500)
	w.Write([]byte(message))
}

func (o *ControllerEnvironmentOptions) stepGitCredentials() error {
	if !o.NoGitCredeentialsInit {
		copy := *o.CommonOptions
		copy.BatchMode = true
		gsc := &StepGitCredentialsOptions{
			StepOptions: StepOptions{
				CommonOptions: &copy,
			},
		}
		err := gsc.Run()
		if err != nil {
			return errors.Wrapf(err, "failed to run: jx step gc credentials")
		}
	}
	return nil
}

// handle request for pipeline runs
func (o *ControllerEnvironmentOptions) handleRequests(w http.ResponseWriter, r *http.Request) {
	eventType, _, _, valid, _ := ValidateWebhook(w, r, o.secret, false)
	if !valid || eventType == "" {
		return
	}
	o.startPipelineRun(w, r)
}

func (o *ControllerEnvironmentOptions) registerWebHook(webhookURL string, secret []byte) error {
	gitURL := o.SourceURL
	log.Infof("verifying that the webhook is registered for the git repository %s\n", util.ColorInfo(gitURL))

	provider, err := o.GitProviderForURL(gitURL, "creating webhook git provider")
	if err != nil {
		return errors.Wrapf(err, "failed to create git provider for git URL %s", gitURL)
	}
	webHookData := &gits.GitWebHookArguments{
		Owner: o.GitOwner,
		Repo: &gits.GitRepository{
			Name: o.GitRepo,
		},
		URL:    webhookURL,
		Secret: string(secret),
	}
	err = provider.CreateWebHook(webHookData)
	if err != nil {
		return errors.Wrapf(err, "failed to create git WebHook provider for URL %s", gitURL)
	}
	return nil
}

// ValidateWebhook ensures that the provided request conforms to the
// format of a Github webhook and the payload can be validated with
// the provided hmac secret. It returns the event type, the event guid,
// the payload of the request, whether the webhook is valid or not,
// and finally the resultant HTTP status code
func ValidateWebhook(w http.ResponseWriter, r *http.Request, hmacSecret []byte, requireGitHubHeaders bool) (string, string, []byte, bool, int) {
	defer r.Body.Close()

	// Our health check uses GET, so just kick back a 200.
	if r.Method == http.MethodGet {
		return "", "", nil, false, http.StatusOK
	}

	// Header checks: It must be a POST with an event type and a signature.
	if r.Method != http.MethodPost {
		responseHTTPError(w, http.StatusMethodNotAllowed, "405 Method not allowed")
		return "", "", nil, false, http.StatusMethodNotAllowed
	}
	eventType := r.Header.Get("X-GitHub-Event")
	eventGUID := r.Header.Get("X-GitHub-Delivery")
	if requireGitHubHeaders {
		if eventType == "" {
			responseHTTPError(w, http.StatusBadRequest, "400 Bad Request: Missing X-GitHub-Event Header")
			return "", "", nil, false, http.StatusBadRequest
		}
		if eventGUID == "" {
			responseHTTPError(w, http.StatusBadRequest, "400 Bad Request: Missing X-GitHub-Delivery Header")
			return "", "", nil, false, http.StatusBadRequest
		}
	} else {
		if eventType == "" {
			eventType = "push"
		}
	}
	sig := r.Header.Get("X-Hub-Signature")
	if sig == "" {
		responseHTTPError(w, http.StatusForbidden, "403 Forbidden: Missing X-Hub-Signature")
		return "", "", nil, false, http.StatusForbidden
	}
	contentType := r.Header.Get("content-type")
	if contentType != "application/json" {
		responseHTTPError(w, http.StatusBadRequest, "400 Bad Request: Hook only accepts content-type: application/json - please reconfigure this hook on GitHub")
		return "", "", nil, false, http.StatusBadRequest
	}
	payload, err := ioutil.ReadAll(r.Body)
	if err != nil {
		responseHTTPError(w, http.StatusInternalServerError, "500 Internal Server Error: Failed to read request body")
		return "", "", nil, false, http.StatusInternalServerError
	}
	// Validate the payload with our HMAC secret.
	if !ValidatePayload(payload, sig, hmacSecret) {
		responseHTTPError(w, http.StatusForbidden, "403 Forbidden: Invalid X-Hub-Signature")
		return "", "", nil, false, http.StatusForbidden
	}
	return eventType, eventGUID, payload, true, http.StatusOK
}

// ValidatePayload ensures that the request payload signature matches the key.
func ValidatePayload(payload []byte, sig string, key []byte) bool {
	if !strings.HasPrefix(sig, "sha1=") {
		return false
	}
	sig = sig[5:]
	sb, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha1.New, key)
	mac.Write(payload)
	expected := mac.Sum(nil)
	return hmac.Equal(sb, expected)
}

// PayloadSignature returns the signature that matches the payload.
func PayloadSignature(payload []byte, key []byte) string {
	mac := hmac.New(sha1.New, key)
	mac.Write(payload)
	sum := mac.Sum(nil)
	return "sha1=" + hex.EncodeToString(sum)
}

func responseHTTPError(w http.ResponseWriter, statusCode int, response string) {
	logrus.WithFields(logrus.Fields{
		"response":    response,
		"status-code": statusCode,
	}).Debug(response)
	http.Error(w, response, statusCode)
}