package local

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/builder/dockerfile"
	"github.com/docker/docker/cliconfig"
	"github.com/docker/docker/daemon/daemonbuilder"
	"github.com/docker/docker/graph"
	"github.com/docker/docker/graph/tags"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/progressreader"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/ulimit"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/runconfig"
	"github.com/docker/docker/utils"
	"golang.org/x/net/context"
)

func (s *router) postCommit(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	if err := httputils.CheckForJSON(r); err != nil {
		return err
	}

	cname := r.Form.Get("container")

	pause := httputils.BoolValue(r, "pause")
	version := httputils.VersionFromContext(ctx)
	if r.FormValue("pause") == "" && version.GreaterThanOrEqualTo("1.13") {
		pause = true
	}

	c, _, err := runconfig.DecodeContainerConfig(r.Body)
	if err != nil && err != io.EOF { //Do not fail if body is empty.
		return err
	}

	commitCfg := &dockerfile.CommitConfig{
		Pause:   pause,
		Repo:    r.Form.Get("repo"),
		Tag:     r.Form.Get("tag"),
		Author:  r.Form.Get("author"),
		Comment: r.Form.Get("comment"),
		Changes: r.Form["changes"],
		Config:  c,
	}

	container, err := s.daemon.Get(cname)
	if err != nil {
		return err
	}

	imgID, err := dockerfile.Commit(container, s.daemon, commitCfg)
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusCreated, &types.ContainerCommitResponse{
		ID: string(imgID),
	})
}

// Creates an image from Pull or from Import
func (s *router) postImagesCreate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var (
		image   = r.Form.Get("fromImage")
		repo    = r.Form.Get("repo")
		tag     = r.Form.Get("tag")
		message = r.Form.Get("message")
	)

	var (
		err    error
		output = ioutils.NewWriteFlusher(w)
	)
	defer output.Close()

	w.Header().Set("Content-Type", "application/json")

	if image != "" { //pull
		if tag == "" {
			image, tag = parsers.ParseRepositoryTag(image)
		}
		metaHeaders := map[string][]string{}
		for k, v := range r.Header {
			if strings.HasPrefix(k, "X-Meta-") {
				metaHeaders[k] = v
			}
		}

		authConfigs := make(map[string]cliconfig.AuthConfig)
		authConfigs, err = s.getAuthConfigs(image, r, false, false)
		if err != nil {
			return err
		}

		imagePullConfig := &graph.ImagePullConfig{
			MetaHeaders: metaHeaders,
			AuthConfigs: authConfigs,
			OutStream:   output,
		}

		err = s.daemon.PullImage(image, tag, imagePullConfig)
	} else { //import
		if tag == "" {
			repo, tag = parsers.ParseRepositoryTag(repo)
		}

		src := r.Form.Get("fromSrc")

		// 'err' MUST NOT be defined within this block, we need any error
		// generated from the download to be available to the output
		// stream processing below
		var newConfig *runconfig.Config
		newConfig, err = dockerfile.BuildFromConfig(&runconfig.Config{}, r.Form["changes"])
		if err != nil {
			return err
		}

		err = s.daemon.ImportImage(src, repo, tag, message, r.Body, output, newConfig)
	}
	if err != nil {
		if !output.Flushed() {
			return err
		}
		sf := streamformatter.NewJSONStreamFormatter()
		output.Write(sf.FormatError(err))
	}

	return nil
}

func (s *router) postImagesPush(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}

	metaHeaders := map[string][]string{}
	for k, v := range r.Header {
		if strings.HasPrefix(k, "X-Meta-") {
			metaHeaders[k] = v
		}
	}
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	name := vars["name"]

	authConfigs, err := s.getAuthConfigs(name, r, true, false)
	if err != nil {
		return err
	}

	output := ioutils.NewWriteFlusher(w)
	defer output.Close()
	imagePushConfig := &graph.ImagePushConfig{
		MetaHeaders: metaHeaders,
		AuthConfigs: authConfigs,
		Tag:         r.Form.Get("tag"),
		Force:       httputils.BoolValue(r, "force"),
		OutStream:   output,
	}

	w.Header().Set("Content-Type", "application/json")

	if err := s.daemon.PushImage(name, imagePushConfig); err != nil {
		if !output.Flushed() {
			return err
		}
		sf := streamformatter.NewJSONStreamFormatter()
		output.Write(sf.FormatError(err))
	}
	return nil
}

func (s *router) getImagesGet(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/x-tar")

	output := ioutils.NewWriteFlusher(w)
	defer output.Close()
	var names []string
	if name, ok := vars["name"]; ok {
		names = []string{name}
	} else {
		names = r.Form["names"]
	}

	if err := s.daemon.ExportImage(names, output); err != nil {
		if !output.Flushed() {
			return err
		}
		sf := streamformatter.NewJSONStreamFormatter()
		output.Write(sf.FormatError(err))
	}
	return nil
}

func (s *router) postImagesLoad(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	return s.daemon.LoadImage(r.Body, w)
}

func (s *router) deleteImages(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}

	name := vars["name"]

	if name == "" {
		return fmt.Errorf("image name cannot be blank")
	}

	force := httputils.BoolValue(r, "force")
	prune := !httputils.BoolValue(r, "noprune")

	list, err := s.daemon.ImageDelete(name, force, prune)
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, list)
}

func (s *router) getImagesByName(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}

	name := vars["name"]

	if httputils.BoolValue(r, "remote") {
		authConfigs, err := s.getAuthConfigs(name, r, false, false)
		if err != nil {
			return err
		}
		metaHeaders := map[string][]string{}
		for k, v := range r.Header {
			if strings.HasPrefix(k, "X-Meta-") {
				metaHeaders[k] = v
			}
		}
		lookupRemoteConfig := &graph.LookupRemoteConfig{
			MetaHeaders: metaHeaders,
			AuthConfigs: authConfigs,
		}
		image, tag := parsers.ParseRepositoryTag(name)
		imageInspect, err := s.daemon.LookupRemote(image, tag, lookupRemoteConfig)
		if err != nil {
			return err
		}
		return httputils.WriteJSON(w, http.StatusOK, imageInspect)
	}

	imageInspect, err := s.daemon.LookupImage(name)
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, imageInspect)
}

func (s *router) getImagesTags(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	var (
		name    = vars["name"]
		tagList *types.RepositoryTagList
		err     error
	)
	if !httputils.BoolValue(r, "remote") {
		tagList, err = s.daemon.Tags(name)
		if err != nil {
			logrus.Warnf("failed to get local tags for %q", name)
		}
	}
	if tagList == nil || err != nil {
		authConfigs, err := s.getAuthConfigs(name, r, false, false)
		if err != nil {
			return err
		}
		metaHeaders := map[string][]string{}
		for k, v := range r.Header {
			if strings.HasPrefix(k, "X-Meta-") {
				metaHeaders[k] = v
			}
		}
		tagsConfig := &graph.RemoteTagsConfig{
			MetaHeaders: metaHeaders,
			AuthConfigs: authConfigs,
		}
		tagList, err = s.daemon.RemoteTags(name, tagsConfig)
		if err != nil {
			logrus.Warnf("failed to get remote tags for %q: %v", name, err)
		}
	}
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, tagList)
}

func (s *router) postBuild(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	var (
		authConfigs        = map[string]cliconfig.AuthConfig{}
		authConfigsEncoded = r.Header.Get("X-Registry-Config")
		buildConfig        = &dockerfile.Config{}
	)

	if authConfigsEncoded != "" {
		authConfigsJSON := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authConfigsEncoded))
		if err := json.NewDecoder(authConfigsJSON).Decode(&authConfigs); err != nil {
			// for a pull it is not an error if no auth was given
			// to increase compatibility with the existing api it is defaulting
			// to be empty.
		}
	}

	w.Header().Set("Content-Type", "application/json")

	version := httputils.VersionFromContext(ctx)
	output := ioutils.NewWriteFlusher(w)
	defer output.Close()
	sf := streamformatter.NewJSONStreamFormatter()
	errf := func(err error) error {
		// Do not write the error in the http output if it's still empty.
		// This prevents from writing a 200(OK) when there is an interal error.
		if !output.Flushed() {
			return err
		}
		_, err = w.Write(sf.FormatError(errors.New(utils.GetErrorMessage(err))))
		if err != nil {
			logrus.Warnf("could not write error response: %v", err)
		}
		return nil
	}

	if httputils.BoolValue(r, "forcerm") && version.GreaterThanOrEqualTo("1.12") {
		buildConfig.Remove = true
	} else if r.FormValue("rm") == "" && version.GreaterThanOrEqualTo("1.12") {
		buildConfig.Remove = true
	} else {
		buildConfig.Remove = httputils.BoolValue(r, "rm")
	}
	if httputils.BoolValue(r, "pull") && version.GreaterThanOrEqualTo("1.16") {
		buildConfig.Pull = true
	}

	repoName, tag := parsers.ParseRepositoryTag(r.FormValue("t"))
	if repoName != "" {
		if err := registry.ValidateRepositoryName(repoName); err != nil {
			return errf(err)
		}
		if len(tag) > 0 {
			if err := tags.ValidateTagName(tag); err != nil {
				return errf(err)
			}
		}
	}

	buildConfig.DockerfileName = r.FormValue("dockerfile")
	buildConfig.Verbose = !httputils.BoolValue(r, "q")
	buildConfig.UseCache = !httputils.BoolValue(r, "nocache")
	buildConfig.ForceRemove = httputils.BoolValue(r, "forcerm")
	buildConfig.MemorySwap = httputils.Int64ValueOrZero(r, "memswap")
	buildConfig.Memory = httputils.Int64ValueOrZero(r, "memory")
	buildConfig.CPUShares = httputils.Int64ValueOrZero(r, "cpushares")
	buildConfig.CPUPeriod = httputils.Int64ValueOrZero(r, "cpuperiod")
	buildConfig.CPUQuota = httputils.Int64ValueOrZero(r, "cpuquota")
	buildConfig.CPUSetCpus = r.FormValue("cpusetcpus")
	buildConfig.CPUSetMems = r.FormValue("cpusetmems")
	buildConfig.CgroupParent = r.FormValue("cgroupparent")

	if r.Form.Get("shmsize") != "" {
		shmSize, err := strconv.ParseInt(r.Form.Get("shmsize"), 10, 64)
		if err != nil {
			return errf(err)
		}
		buildConfig.ShmSize = shmSize
	}

	var buildUlimits = []*ulimit.Ulimit{}
	ulimitsJSON := r.FormValue("ulimits")
	if ulimitsJSON != "" {
		if err := json.NewDecoder(strings.NewReader(ulimitsJSON)).Decode(&buildUlimits); err != nil {
			return errf(err)
		}
		buildConfig.Ulimits = buildUlimits
	}

	var buildArgs = map[string]string{}
	buildArgsJSON := r.FormValue("buildargs")
	if buildArgsJSON != "" {
		if err := json.NewDecoder(strings.NewReader(buildArgsJSON)).Decode(&buildArgs); err != nil {
			return errf(err)
		}
		buildConfig.BuildArgs = buildArgs
	}

	remoteURL := r.FormValue("remote")

	// Currently, only used if context is from a remote url.
	// The field `In` is set by DetectContextFromRemoteURL.
	// Look at code in DetectContextFromRemoteURL for more information.
	pReader := &progressreader.Config{
		// TODO: make progressreader streamformatter-agnostic
		Out:       output,
		Formatter: sf,
		Size:      r.ContentLength,
		NewLines:  true,
		ID:        "Downloading context",
		Action:    remoteURL,
	}

	var (
		context        builder.ModifiableContext
		dockerfileName string
		err            error
	)
	context, dockerfileName, err = daemonbuilder.DetectContextFromRemoteURL(r.Body, remoteURL, pReader)
	if err != nil {
		return errf(err)
	}
	defer func() {
		if err := context.Close(); err != nil {
			logrus.Debugf("[BUILDER] failed to remove temporary context: %v", err)
		}
	}()

	uidMaps, gidMaps := s.daemon.GetUIDGIDMaps()
	defaultArchiver := &archive.Archiver{
		Untar:   chrootarchive.Untar,
		UIDMaps: uidMaps,
		GIDMaps: gidMaps,
	}
	docker := daemonbuilder.Docker{s.daemon, output, authConfigs, defaultArchiver}

	b, err := dockerfile.NewBuilder(buildConfig, docker, builder.DockerIgnoreContext{context}, nil)
	if err != nil {
		return errf(err)
	}
	b.Stdout = &streamformatter.StdoutFormatter{Writer: output, StreamFormatter: sf}
	b.Stderr = &streamformatter.StderrFormatter{Writer: output, StreamFormatter: sf}

	if closeNotifier, ok := w.(http.CloseNotifier); ok {
		finished := make(chan struct{})
		defer close(finished)
		go func() {
			select {
			case <-finished:
			case <-closeNotifier.CloseNotify():
				logrus.Infof("Client disconnected, cancelling job: build")
				b.Cancel()
			}
		}()
	}

	if len(dockerfileName) > 0 {
		b.DockerfileName = dockerfileName
	}

	imgID, err := b.Build()
	if err != nil {
		return errf(err)
	}

	if repoName != "" {
		if err := s.daemon.TagImage(repoName, tag, string(imgID), true, true); err != nil {
			return errf(err)
		}
	}

	return nil
}

func (s *router) getImagesJSON(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	// FIXME: The filter parameter could just be a match filter
	images, err := s.daemon.ListImages(r.Form.Get("filters"), r.Form.Get("filter"), httputils.BoolValue(r, "all"))
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, images)
}

func (s *router) getImagesHistory(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}

	name := vars["name"]
	history, err := s.daemon.ImageHistory(name)
	if err != nil {
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, history)
}

func (s *router) postImagesTag(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	if vars == nil {
		return fmt.Errorf("Missing parameter")
	}

	repo := r.Form.Get("repo")
	tag := r.Form.Get("tag")
	name := vars["name"]
	force := httputils.BoolValue(r, "force")
	if err := s.daemon.TagImage(repo, tag, name, force, true); err != nil {
		return err
	}
	s.daemon.EventsService.Log("tag", utils.ImageReference(repo, tag), "")
	w.WriteHeader(http.StatusCreated)
	return nil
}

func (s *router) getImagesSearch(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	var (
		headers = map[string][]string{}
		term    = r.Form.Get("term")
	)

	authConfigs, err := s.getAuthConfigs(term, r, false, true)
	if err != nil {
		return err
	}

	for k, v := range r.Header {
		if strings.HasPrefix(k, "X-Meta-") {
			headers[k] = v
		}
	}
	results, err := s.daemon.SearchRegistryForImages(term, authConfigs, headers, httputils.BoolValue(r, "noIndex"))
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, results)
}

func (s *router) getAuthConfigs(name string, r *http.Request, backward bool, searchTerm bool) (map[string]cliconfig.AuthConfig, error) {
	authEncoded := r.Header.Get("X-Registry-Auth")
	authConfigs := make(map[string]cliconfig.AuthConfig)
	if authEncoded != "" {
		authJSON := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authEncoded))
		if err := json.NewDecoder(authJSON).Decode(&authConfigs); err != nil {
			// for a pull it is not an error if no auth was given
			// to increase compatibility with the existing api it is defaulting to be empty
			authConfigs = make(map[string]cliconfig.AuthConfig)
		}
	}
	// maybe client just sends one auth config
	// try to resolve just one auth config...
	authConfig := cliconfig.AuthConfig{}
	if len(authConfigs) == 0 {
		if authEncoded != "" {
			authJSONSingle := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authEncoded))
			if err := json.NewDecoder(authJSONSingle).Decode(&authConfig); err != nil {
				// for a pull it is not an error if no auth was given
				// to increase compatibility with the existing api it is defaulting to be empty
				authConfig = cliconfig.AuthConfig{}
			}
		} else if backward {
			// the old format is supported for compatibility if there was no authConfig header
			if err := json.NewDecoder(r.Body).Decode(&authConfig); err != nil {
				return nil, fmt.Errorf("Bad parameters and missing X-Registry-Auth: %v", err)
			}
		}
	}

	if len(authConfigs) == 0 {
		var (
			repoInfo *registry.RepositoryInfo
			err      error
		)
		if searchTerm {
			repoInfo, err = s.daemon.RegistryService.ResolveRepositoryBySearch(name)
			if err != nil {
				return nil, err
			}
		} else {
			repoInfo, err = registry.ParseRepositoryInfo(name)
			if err != nil {
				return nil, err
			}
		}
		// this is the case when we fully qualify images
		authConfigKey := repoInfo.Index.GetAuthConfigKey()
		authConfigs[authConfigKey] = authConfig
	}

	return authConfigs, nil
}