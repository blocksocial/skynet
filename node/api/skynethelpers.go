package api

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/ratelimit"
	"gitlab.com/SkynetLabs/skyd/build"
	"gitlab.com/SkynetLabs/skyd/skykey"
	"gitlab.com/SkynetLabs/skyd/skymodules"
	"gitlab.com/SkynetLabs/skyd/skymodules/renter"
	"go.sia.tech/siad/crypto"
	"go.sia.tech/siad/modules"
	"go.sia.tech/siad/types"
)

var (
	// errIncompleteRangeRequest is the error returned when the range
	// request is incomplete.
	errIncompleteRangeRequest = errors.New("the 'start' and 'end' params must be both blank or provided")

	// errInvalidRangeParams is the error returned when the range params are
	// invalid.
	errInvalidRangeParams = errors.New("'start' param should be less than or equal to 'end' param")

	// errRangeSetTwice is the error returned when the range is set twice,
	// once in the Header and once in the query params
	errRangeSetTwice = errors.New("range request should use either the Header or the query params but not both")
)

type (
	// skyfileUploadParams is a helper struct that contains all of the query
	// string parameters on download
	skyfileDownloadParams struct {
		attachment           bool
		format               skymodules.SkyfileFormat
		includeLayout        bool
		path                 string
		pricePerMS           types.Currency
		skylink              skymodules.Skylink
		skylinkStringNoQuery string
		timeout              time.Duration
	}

	// skyfileUploadParams is a helper struct that contains all of the query
	// string parameters on upload
	skyfileUploadParams struct {
		baseChunkRedundancy uint8
		defaultPath         string
		convertPath         string
		disableDefaultPath  bool
		tryFiles            []string
		errorPages          map[int]string
		dryRun              bool
		filename            string
		force               bool
		mode                os.FileMode
		monetization        *skymodules.Monetization
		root                bool
		siaPath             skymodules.SiaPath
		skyKeyID            skykey.SkykeyID
		skyKeyName          string
	}

	// skyfileUploadHeaders is a helper struct that contains all of the request
	// headers on upload
	skyfileUploadHeaders struct {
		mediaType    string
		disableForce bool
	}
)

// writeReader is a helper type that turns a writer into a io.WriteReader.
type writeReader struct {
	io.Writer
}

// Read implements the io.Reader interface but returns 0 and EOF.
func (wr *writeReader) Read(_ []byte) (int, error) {
	build.Critical("Read method of the writeReader is not intended to be used")
	return 0, io.EOF
}

// monetizedResponseWriter is a wrapper for a response writer. It monetizes the
// returned bytes.
type monetizedResponseWriter struct {
	staticInner http.ResponseWriter
	staticW     io.Writer
}

// newMonetizedResponseWriter creates a new response writer wrapped with a
// monetized writer.
func newMonetizedResponseWriter(inner http.ResponseWriter, md skymodules.SkyfileMetadata, wallet modules.SiacoinSenderMulti, cr map[string]types.Currency, mb types.Currency) http.ResponseWriter {
	return &monetizedResponseWriter{
		staticInner: inner,
		staticW:     newMonetizedWriter(inner, md, wallet, cr, mb),
	}
}

// Header calls the inner writers Header method.
func (rw *monetizedResponseWriter) Header() http.Header {
	return rw.staticInner.Header()
}

// WriteHeader calls the inner writers WriteHeader method.
func (rw *monetizedResponseWriter) WriteHeader(statusCode int) {
	rw.staticInner.WriteHeader(statusCode)
}

// Write writes to the underlying monetized writer.
func (rw *monetizedResponseWriter) Write(b []byte) (int, error) {
	return rw.staticW.Write(b)
}

// monetizedWriter is a wrapper for an io.Writer. It monetizes the returned
// bytes.
type monetizedWriter struct {
	staticW      io.Writer
	staticMD     skymodules.SkyfileMetadata
	staticWallet modules.SiacoinSenderMulti

	staticConversionRates  map[string]types.Currency
	staticMonetizationBase types.Currency

	// count is used for sanity checking the number of monetized bytes against
	// the total.
	count int
}

// newMonetizedWriter creates a new wrapped writer.
func newMonetizedWriter(w io.Writer, md skymodules.SkyfileMetadata, wallet modules.SiacoinSenderMulti, cr map[string]types.Currency, mb types.Currency) io.Writer {
	// Ratelimit the writer.
	rl := ratelimit.NewRateLimit(0, 0, 0)
	return &monetizedWriter{
		staticW:                ratelimit.NewRLReadWriter(&writeReader{Writer: w}, rl, make(chan struct{})),
		staticMD:               md,
		staticWallet:           wallet,
		staticConversionRates:  cr,
		staticMonetizationBase: mb,
	}
}

// ReadFrom implements the io.ReaderFrom interface for the
// monetizedResponseWriter. This allows us to overwrite the default fetch size
// of 32kib of http.ServeContent with something more appropriate for the way we
// fetch data from Sia.
func (rw *monetizedResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, renter.SkylinkDataSourceRequestSize)
	return io.CopyBuffer(rw.staticW, r, buf)
}

// Write wraps the inner Write and adds monetization.
func (rw *monetizedWriter) Write(b []byte) (int, error) {
	// Handle legacy uploads with length 0 by passing it through to the inner
	// writer.
	if rw.staticMD.Length == 0 && rw.staticMD.Monetization == nil {
		return rw.staticW.Write(b)
	}

	// Sanity check the number of monetized bytes against the total.
	rw.count += len(b)
	if rw.count > int(rw.staticMD.Length) {
		err := fmt.Errorf("monetized more data than the total data of the skylink: %v > %v", rw.count, rw.staticMD.Length)
		build.Critical(err)
		return 0, err
	}

	// Forward data to inner.
	// TODO: instead of directly writing to the ratelimited writer, write to a
	// not ratelimited buffer on disk which forwards the data to the writer.
	// Otherwise we are starving the renter.
	n, err := rw.staticW.Write(b)
	if err != nil {
		return n, err
	}

	// Pay monetizers.
	if build.Release == "testing" {
		err := skymodules.PayMonetizers(rw.staticWallet, rw.staticMD.Monetization, uint64(len(b)), rw.staticMD.Length, rw.staticConversionRates, rw.staticMonetizationBase)
		if err != nil {
			return 0, err
		}
	}
	return n, nil
}

// customStatusResponseWriter is a wrapper for a response writer. It returns
// a custom status code instead of 200 OK.
type customStatusResponseWriter struct {
	// TODO Do I need to lock this on write?
	inner            http.ResponseWriter
	staticErrorPages map[int]string
	staticMetadata   skymodules.SkyfileMetadata
	staticStreamer   skymodules.SkyfileStreamer
	staticRequest    *http.Request
	statusSent       bool
}

// newCustomStatusResponseWriter creates a new customStatusResponseWriter.
func newCustomStatusResponseWriter(inner http.ResponseWriter, r *http.Request, meta skymodules.SkyfileMetadata, streamer skymodules.SkyfileStreamer, statusSent bool) http.ResponseWriter {
	return &customStatusResponseWriter{
		inner:            inner,
		staticErrorPages: meta.ErrorPages,
		staticMetadata:   meta,
		staticStreamer:   streamer,
		staticRequest:    r,
		statusSent:       statusSent,
	}
}

// Header calls the inner writers Header method.
func (rw *customStatusResponseWriter) Header() http.Header {
	return rw.inner.Header()
}

// WriteHeader calls the inner writers WriteHeader method if there is an
// errorpage specified for this status code, it will also extract its content
// and write it to the inner writer as well.
func (rw *customStatusResponseWriter) WriteHeader(status int) {
	if rw.statusSent {
		return
	}
	rw.statusSent = true

	fmt.Println(">>> ", status)
	fmt.Printf(">>> rw %+v\n", rw)
	fmt.Printf(">>> ep %+v\n", rw.staticErrorPages)

	errpath, exists := rw.staticErrorPages[status]
	if !exists {
		rw.inner.WriteHeader(status)
		return
	}

	metadataForPath, _, offset, size := rw.staticMetadata.ForPath(errpath)
	if len(metadataForPath.Subfiles) == 0 {
		fmt.Println(">>> ER no subfiles that match this errpath")
		WriteError(rw.inner, Error{fmt.Sprintf("CUSTOM: failed to download contents for errpath: %v", errpath)}, http.StatusNotFound)
		return
	}
	rawMetadataForPath, err := json.Marshal(metadataForPath)
	if err != nil {
		fmt.Println(">>> ER meta for errpath", err)
		WriteError(rw.inner, Error{fmt.Sprintf("CUSTOM: failed to marshal subfile metadata for errpath %v", errpath)}, http.StatusNotFound)
		return
	}
	streamer, err := NewLimitStreamer(rw.staticStreamer, metadataForPath, rawMetadataForPath, rw.staticStreamer.Skylink(), rw.staticStreamer.Layout(), offset, size)
	if err != nil {
		fmt.Println(">>> ER limit streamer", err)
		WriteError(rw.inner, Error{fmt.Sprintf("CUSTOM: failed to download contents for errpath: %v, could not create limit streamer", errpath)}, http.StatusInternalServerError)
		return
	}
	rw.inner.WriteHeader(status)

	// TODO content type header
	// b := make([]byte, size)
	// n, err := streamer.Read(b)
	// if err != nil {
	// 	panic(err)
	// }
	// fmt.Println("read", n, "bytes from streamer:", string(b))
	// n, err = fmt.Fprint(rw.inner, string(b))
	// if err != nil {
	// 	build.Critical("error writing errorpage content", err)
	// }
	// fmt.Println("successfully wrote to writer", n)
	// rw.inner = newNopWriter(rw.inner)
	// fmt.Println("replaced inner writer with a nop")

	// we send the data on the same writer, which will no longer accept
	// status headers.
	http.ServeContent(rw, rw.staticRequest, rw.staticMetadata.Filename, time.Time{}, streamer)
}

// Write calls the inner writer Write method.
func (rw *customStatusResponseWriter) Write(b []byte) (int, error) {
	return rw.inner.Write(b)
}

type nopWriter struct {
	inner http.ResponseWriter
}

// nopWriter is a no-op ResponseWriter replacement that reads the headers from
// the underlying ResponseWriter but does not write anything.
func newNopWriter(inner http.ResponseWriter) nopWriter {
	return nopWriter{
		inner: inner,
	}
}

func (rw nopWriter) Header() http.Header {
	return rw.inner.Header()
}

func (rw nopWriter) Write(b []byte) (int, error) {
	return len(b), nil
}

func (rw nopWriter) WriteHeader(_ int) {}

// buildETag is a helper function that returns an ETag.
func buildETag(skylink skymodules.Skylink, path string, format skymodules.SkyfileFormat) string {
	return crypto.HashAll(
		skylink.String(),
		path,
		string(format),
		"1", // random variable to cache bust all existing ETags (SkylinkV2 fix)
	).String()
}

// isMultipartRequest is a helper method that checks if the given media type
// matches that of a multipart form.
func isMultipartRequest(mediaType string) bool {
	return strings.HasPrefix(mediaType, "multipart/form-data")
}

// parseSkylinkURL splits a raw skylink URL into its components - a skylink, a
// string representation of the skylink with the query parameters stripped, and
// a path. The input skylink URL should not have been URL-decoded. The path is
// URL-decoded before returning as it is for us to parse and use, while the
// other components remain encoded for the skapp.
func parseSkylinkURL(skylinkURL, apiRoute string) (skylink skymodules.Skylink, skylinkStringNoQuery, path string, err error) {
	s := strings.TrimPrefix(skylinkURL, apiRoute)
	s = strings.TrimPrefix(s, "/")
	// Parse out optional path to a subfile
	path = "/" // default to root
	splits := strings.SplitN(s, "?", 2)
	skylinkStringNoQuery = splits[0]
	splits = strings.SplitN(skylinkStringNoQuery, "/", 2)
	// Check if a path is passed.
	if len(splits) > 1 && len(splits[1]) > 0 {
		path = skymodules.EnsurePrefix(splits[1], "/")
	}
	// Decode the path as it may contain URL-encoded characters.
	path, err = url.QueryUnescape(path)
	if err != nil {
		return
	}
	// Parse skylink
	err = skylink.LoadString(s)
	return
}

// parseTimeout tries to parse the timeout from the query string and validate
// it. If not present, it will default to DefaultSkynetRequestTimeout.
func parseTimeout(queryForm url.Values) (time.Duration, error) {
	timeoutStr := queryForm.Get("timeout")
	if timeoutStr == "" {
		return DefaultSkynetRequestTimeout, nil
	}

	timeoutInt, err := strconv.Atoi(timeoutStr)
	if err != nil {
		return 0, errors.AddContext(err, "unable to parse 'timeout'")
	}
	if timeoutInt > MaxSkynetRequestTimeout {
		return 0, errors.AddContext(err, fmt.Sprintf("'timeout' parameter too high, maximum allowed timeout is %ds", MaxSkynetRequestTimeout))
	}
	return time.Duration(timeoutInt) * time.Second, nil
}

// parseDownloadRequestParameters is a helper function that parses all of the
// query parameters from a download request
func parseDownloadRequestParameters(req *http.Request) (*skyfileDownloadParams, error) {
	// Parse the skylink from the raw URL of the request. Any special characters
	// in the raw URL are encoded, allowing us to differentiate e.g. the '?'
	// that begins query parameters from the encoded version '%3F'.
	skylink, skylinkStringNoQuery, path, err := parseSkylinkURL(req.URL.String(), "/skynet/skylink/")
	if err != nil {
		return nil, fmt.Errorf("error parsing skylink: %v", err)
	}

	// Parse the query params.
	queryForm, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return nil, errors.New("failed to parse query params")
	}

	// Parse the 'attachment' query string parameter.
	var attachment bool
	attachmentStr := queryForm.Get("attachment")
	if attachmentStr != "" {
		attachment, err = strconv.ParseBool(attachmentStr)
		if err != nil {
			return nil, fmt.Errorf("unable to parse 'attachment' parameter: %v", err)
		}
	}

	// Parse the 'format' query string parameter.
	format := skymodules.SkyfileFormat(strings.ToLower(queryForm.Get("format")))
	switch format {
	case skymodules.SkyfileFormatNotSpecified:
	case skymodules.SkyfileFormatConcat:
	case skymodules.SkyfileFormatTar:
	case skymodules.SkyfileFormatTarGz:
	case skymodules.SkyfileFormatZip:
	default:
		return nil, errors.New("unable to parse 'format' parameter, allowed values are: 'concat', 'tar', 'targz' and 'zip'")
	}

	// Parse the `include-layout` query string parameter.
	var includeLayout bool
	includeLayoutStr := queryForm.Get("include-layout")
	if includeLayoutStr != "" {
		includeLayout, err = strconv.ParseBool(includeLayoutStr)
		if err != nil {
			return nil, fmt.Errorf("unable to parse 'include-layout' parameter: %v", err)
		}
	}

	// Parse the timeout.
	timeout, err := parseTimeout(queryForm)
	if err != nil {
		return nil, err
	}

	// Parse pricePerMS.
	pricePerMS := DefaultSkynetPricePerMS
	pricePerMSStr := queryForm.Get("priceperms")
	if pricePerMSStr != "" {
		_, err = fmt.Sscan(pricePerMSStr, &pricePerMS)
		if err != nil {
			return nil, fmt.Errorf("unable to parse 'pricePerMS' parameter: %v", err)
		}
	}

	// Parse a range request from the query form
	startStr := queryForm.Get("start")
	endStr := queryForm.Get("end")
	var start, end uint64
	rangeParam := startStr != "" && endStr != ""
	if rangeParam {
		// Verify we don't have a range request in both the Header and the params
		headerRange := req.Header.Get("Range")
		if headerRange != "" {
			return nil, errRangeSetTwice
		}
		// Parse start param
		start, err = strconv.ParseUint(startStr, 10, 64)
		if err != nil {
			return nil, errors.AddContext(err, "unable to parse 'start' parameter")
		}
		// Parse end param
		end, err = strconv.ParseUint(endStr, 10, 64)
		if err != nil {
			return nil, errors.AddContext(err, "unable to parse 'end' parameter")
		}
		// Check that start is not greater than end. It is ok for end to
		// equal start as that would indicate a request for a single
		// byte.
		if start > end {
			return nil, errInvalidRangeParams
		}

		// Set the Range field in the Header
		AddRangeHeaderToRequest(req, start, end)
	} else if startStr != "" || endStr != "" {
		return nil, errIncompleteRangeRequest
	}

	return &skyfileDownloadParams{
		attachment:           attachment,
		format:               format,
		includeLayout:        includeLayout,
		path:                 path,
		pricePerMS:           pricePerMS,
		skylink:              skylink,
		skylinkStringNoQuery: skylinkStringNoQuery,
		timeout:              timeout,
	}, nil
}

// parseUploadHeadersAndRequestParameters is a helper function that parses all
// the query parameters and headers from an upload request
func parseUploadHeadersAndRequestParameters(req *http.Request, ps httprouter.Params) (*skyfileUploadHeaders, *skyfileUploadParams, error) {
	var err error

	// parse 'Skynet-Disable-Force' request header
	var disableForce bool
	strDisableForce := req.Header.Get("Skynet-Disable-Force")
	if strDisableForce != "" {
		disableForce, err = strconv.ParseBool(strDisableForce)
		if err != nil {
			return nil, nil, errors.AddContext(err, "unable to parse 'Skynet-Disable-Force' header")
		}
	}

	// parse 'Content-Type' request header
	ct := req.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, nil, errors.AddContext(err, "failed parsing 'Content-Type' header")
	}

	// parse query
	queryForm, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return nil, nil, errors.AddContext(err, "failed to parse query")
	}

	// parse 'basechunkredundancy' query parameter
	baseChunkRedundancy := uint8(0)
	if rStr := queryForm.Get("basechunkredundancy"); rStr != "" {
		if _, err := fmt.Sscan(rStr, &baseChunkRedundancy); err != nil {
			return nil, nil, errors.AddContext(err, "unable to parse 'basechunkredundancy' parameter")
		}
	}

	// parse 'convertpath' query parameter
	convertPath := queryForm.Get("convertpath")

	// parse 'defaultpath' query parameter
	defaultPath := queryForm.Get("defaultpath")
	if defaultPath != "" {
		defaultPath = skymodules.EnsurePrefix(defaultPath, "/")
	}

	// parse 'disabledefaultpath' query parameter
	var disableDefaultPath bool
	disableDefaultPathStr := queryForm.Get("disabledefaultpath")
	if disableDefaultPathStr != "" {
		disableDefaultPath, err = strconv.ParseBool(disableDefaultPathStr)
		if err != nil {
			return nil, nil, errors.AddContext(err, "unable to parse 'disabledefaultpath' parameter")
		}
	}

	// parse 'tryfiles' query parameter
	tryFiles, err := ParseTryFiles(queryForm.Get("tryfiles"))
	if err != nil {
		return nil, nil, errors.AddContext(err, "unable to parse 'tryfiles' parameter")
	}
	// if we don't have any tryfiles defined, and we don't have a defaultpath or
	// disabledefaultpath, we want to default to tryfiles with index.html
	if len(tryFiles) == 0 && defaultPath == "" && disableDefaultPath == false {
		tryFiles = []string{"index.html"}
	}

	if (defaultPath != "" || disableDefaultPath) && len(tryFiles) > 0 {
		return nil, nil, errors.New("defaultpath and disabledefaultpath are not compatible with tryfiles")
	}

	errPages, err := ParseErrorPages(queryForm.Get("errorpages"))
	if err != nil {
		return nil, nil, errors.AddContext(err, "invalid errorpages parameter")
	}

	// parse 'dryrun' query parameter
	var dryRun bool
	dryRunStr := queryForm.Get("dryrun")
	if dryRunStr != "" {
		dryRun, err = strconv.ParseBool(dryRunStr)
		if err != nil {
			return nil, nil, errors.AddContext(err, "unable to parse 'dryrun' parameter")
		}
	}

	// parse 'filename' query parameter
	filename := queryForm.Get("filename")

	// parse 'force' query parameter
	var force bool
	strForce := queryForm.Get("force")
	if strForce != "" {
		force, err = strconv.ParseBool(strForce)
		if err != nil {
			return nil, nil, errors.AddContext(err, "unable to parse 'force' parameter")
		}
	}

	// parse 'mode' query parameter
	modeStr := queryForm.Get("mode")
	var mode os.FileMode
	if modeStr != "" {
		_, err := fmt.Sscanf(modeStr, "%o", &mode)
		if err != nil {
			return nil, nil, errors.AddContext(err, "unable to parse 'mode' parameter")
		}
	}

	// parse 'root' query parameter
	var root bool
	rootStr := queryForm.Get("root")
	if rootStr != "" {
		root, err = strconv.ParseBool(rootStr)
		if err != nil {
			return nil, nil, errors.AddContext(err, "unable to parse 'root' parameter")
		}
	}

	// parse 'siapath' query parameter
	var siaPath skymodules.SiaPath
	siaPathStr := ps.ByName("siapath")
	if root {
		siaPath, err = skymodules.NewSiaPath(siaPathStr)
	} else {
		siaPath, err = skymodules.SkynetFolder.Join(siaPathStr)
	}
	if err != nil {
		return nil, nil, errors.AddContext(err, "unable to parse 'siapath' parameter")
	}

	// parse 'skykeyname' query parameter
	skykeyName := queryForm.Get("skykeyname")

	// parse 'skykeyid' query parameter
	var skykeyID skykey.SkykeyID
	skykeyIDStr := queryForm.Get("skykeyid")
	if skykeyIDStr != "" {
		err = skykeyID.FromString(skykeyIDStr)
		if err != nil {
			return nil, nil, errors.AddContext(err, "unable to parse 'skykeyid'")
		}
	}

	// parse 'monetization'.
	var monetization *skymodules.Monetization
	monetizationStr := queryForm.Get("monetization")
	if monetizationStr != "" {
		var m skymodules.Monetization
		err = json.Unmarshal([]byte(monetizationStr), &m)
		if err != nil {
			return nil, nil, errors.AddContext(err, "unable to parse 'monetizers'")
		}
		if err := skymodules.ValidateMonetization(&m); err != nil {
			return nil, nil, err
		}
		monetization = &m
	}

	// validate parameter combos

	// verify force is not set if disable force header was set
	if disableForce && force {
		return nil, nil, errors.New("'force' has been disabled on this node")
	}

	// verify the dry-run and force parameter are not combined
	if !disableForce && force && dryRun {
		return nil, nil, errors.New("'dryRun' and 'force' can not be combined")
	}

	// verify disabledefaultpath and defaultpath are not combined
	if disableDefaultPath && defaultPath != "" {
		return nil, nil, errors.AddContext(skymodules.ErrInvalidDefaultPath, "DefaultPath and DisableDefaultPath are mutually exclusive and cannot be set together")
	}

	// verify default path params are not set if it's not a multipart upload
	if !isMultipartRequest(mediaType) && (disableDefaultPath || defaultPath != "") {
		return nil, nil, errors.New("DefaultPath and DisableDefaultPath can only be set on multipart uploads")
	}

	// verify convertpath and filename are not combined
	if convertPath != "" && filename != "" {
		return nil, nil, errors.New("cannot set both a 'convertpath' and a 'filename'")
	}

	// verify skykeyname and skykeyid are not combined
	if skykeyName != "" && skykeyIDStr != "" {
		return nil, nil, errors.New("cannot set both a 'skykeyname' and 'skykeyid'")
	}

	// create headers and parameters
	headers := &skyfileUploadHeaders{
		disableForce: disableForce,
		mediaType:    mediaType,
	}
	params := &skyfileUploadParams{
		baseChunkRedundancy: baseChunkRedundancy,
		convertPath:         convertPath,
		defaultPath:         defaultPath,
		disableDefaultPath:  disableDefaultPath,
		dryRun:              dryRun,
		errorPages:          errPages,
		filename:            filename,
		force:               force,
		mode:                mode,
		monetization:        monetization,
		root:                root,
		siaPath:             siaPath,
		skyKeyID:            skykeyID,
		skyKeyName:          skykeyName,
		tryFiles:            tryFiles,
	}
	return headers, params, nil
}

// serveArchive serves skyfiles as an archive by reading them from r and writing
// the archive to dst using the given archiveFunc.
func serveArchive(w http.ResponseWriter, src io.ReadSeeker, format skymodules.SkyfileFormat, md skymodules.SkyfileMetadata, monetize func(io.Writer) io.Writer) (err error) {
	// Based upon the given format, set the Content-Type header, wrap the writer
	// and select an archive function.
	var dst io.Writer
	var archiveFunc archiveFunc
	switch format {
	case skymodules.SkyfileFormatTar:
		archiveFunc = serveTar
		w.Header().Set("Content-Type", "application/x-tar")
		dst = w
	case skymodules.SkyfileFormatTarGz:
		archiveFunc = serveTar
		w.Header().Set("Content-Type", "application/gzip")
		gzw := gzip.NewWriter(w)
		defer func() {
			err = errors.Compose(err, gzw.Close())
		}()
		dst = gzw
	case skymodules.SkyfileFormatZip:
		archiveFunc = serveZip
		w.Header().Set("Content-Type", "application/zip")
		dst = w
	}

	// Get the files to archive.
	var files []skymodules.SkyfileSubfileMetadata
	for _, file := range md.Subfiles {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Offset < files[j].Offset
	})
	// If there are no files, it's a single file download. Manually construct a
	// SkyfileSubfileMetadata from the SkyfileMetadata.
	if len(files) == 0 {
		length := md.Length
		if md.Length == 0 {
			// Fetch the length of the file by seeking to the end and then back
			// to the start.
			seekLen, err := src.Seek(0, io.SeekEnd)
			if err != nil {
				return errors.AddContext(err, "serveArchive: failed to seek to end of skyfile")
			}

			// v150Compat a missing length is fine for legacy links but new
			// links should always have the length set.
			if build.Release == "testing" && seekLen != 0 {
				build.Critical("SkyfileMetadata is missing length")
			}

			// Seek back to the start
			_, err = src.Seek(0, io.SeekStart)
			if err != nil {
				return errors.AddContext(err, "serveArchive: failed to seek to start of skyfile")
			}
			length = uint64(seekLen)
		}
		// Construct the SkyfileSubfileMetadata.
		files = append(files, skymodules.SkyfileSubfileMetadata{
			FileMode: md.Mode,
			Filename: md.Filename,
			Offset:   0,
			Len:      length,
		})
	}
	err = archiveFunc(dst, src, files, monetize)
	return err
}

// serveTar is an archiveFunc that implements serving the files from src to dst
// as a tar.
func serveTar(dst io.Writer, src io.Reader, files []skymodules.SkyfileSubfileMetadata, monetize func(io.Writer) io.Writer) error {
	tw := tar.NewWriter(dst)
	for _, file := range files {
		// Create header.
		header, err := tar.FileInfoHeader(file, file.Name())
		if err != nil {
			return err
		}
		// Modify name to match path within skyfile.
		header.Name = file.Filename
		// Write header.
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		// Write file content.
		if _, err := io.CopyN(monetize(tw), src, header.Size); err != nil {
			return err
		}
	}
	return tw.Close()
}

// serveZip is an archiveFunc that implements serving the files from src to dst
// as a zip.
func serveZip(dst io.Writer, src io.Reader, files []skymodules.SkyfileSubfileMetadata, monetize func(io.Writer) io.Writer) error {
	zw := zip.NewWriter(dst)
	for _, file := range files {
		f, err := zw.Create(file.Filename)
		if err != nil {
			return errors.AddContext(err, "serveZip: failed to add the file to the zip")
		}

		// Write file content.
		_, err = io.CopyN(monetize(f), src, int64(file.Len))
		if err != nil {
			return errors.AddContext(err, "serveZip: failed to write file contents to the zip")
		}
	}
	return zw.Close()
}

// handleSkynetError is a handler that returns the correct status code for a
// given error returned by a skynet related method.
func handleSkynetError(w http.ResponseWriter, prefix string, err error) {
	httpErr := Error{fmt.Sprintf("%v: %v", prefix, err)}

	if errors.Contains(err, renter.ErrSkylinkBlocked) {
		WriteError(w, httpErr, http.StatusUnavailableForLegalReasons)
		return
	}
	if errors.Contains(err, renter.ErrRootNotFound) {
		WriteError(w, httpErr, http.StatusNotFound)
		return
	}
	if errors.Contains(err, renter.ErrRegistryEntryNotFound) {
		WriteError(w, httpErr, http.StatusNotFound)
		return
	}
	if errors.Contains(err, renter.ErrRegistryLookupTimeout) {
		WriteError(w, httpErr, http.StatusNotFound)
		return
	}
	if errors.Contains(err, skymodules.ErrMalformedSkylink) {
		WriteError(w, httpErr, http.StatusBadRequest)
		return
	}
	if errors.Contains(err, renter.ErrInvalidSkylinkVersion) {
		WriteError(w, httpErr, http.StatusBadRequest)
		return
	}
	if err != nil {
		WriteError(w, httpErr, http.StatusInternalServerError)
		return
	}
}

// attachRegistryEntryProof takes a number of registry entries and parses them.
// The result is then attached to an API response for the client to verify the
// response against.
func attachRegistryEntryProof(w http.ResponseWriter, srvs []skymodules.RegistryEntry) error {
	proofChain := make([]RegistryHandlerGET, 0, len(srvs))
	for _, srv := range srvs {
		proofChain = append(proofChain, RegistryHandlerGET{
			Data:      hex.EncodeToString(srv.Data),
			DataKey:   srv.Tweak,
			Revision:  srv.Revision,
			PublicKey: srv.PubKey,
			Signature: hex.EncodeToString(srv.Signature[:]),
			Type:      srv.Type,
		})
	}
	b, err := json.Marshal(proofChain)
	if err != nil {
		return err
	}
	w.Header().Set("Skynet-Proof", string(b))
	return nil
}

// ParseErrorPages unmarshals an errorpages string into an map[int]string.
func ParseErrorPages(s string) (map[int]string, error) {
	if len(s) == 0 {
		return map[int]string{}, nil
	}
	errPages := &map[int]string{}
	err := json.Unmarshal([]byte(s), errPages)
	if err != nil {
		return nil, errors.AddContext(err, "invalid errorpages value")
	}
	return *errPages, nil
}

// ParseTryFiles unmarshals a tryfiles string.
// TODO unit test
func ParseTryFiles(s string) ([]string, error) {
	if len(s) == 0 {
		return []string{}, nil
	}
	var tf []string
	err := json.Unmarshal([]byte(s), &tf)
	if err != nil {
		return nil, errors.AddContext(err, "invalid tryfiles value")
	}
	return tf, nil
}

// determinePathBasedOnTryfiles determines if we should serve a different path
// based on the given metadata. It also returns a boolean which tells us whether
// the returned path is different from the provided path.
// TODO Unit tests.
func determinePathBasedOnTryfiles(path string, subfiles skymodules.SkyfileSubfiles, tryfiles []string) (string, bool) {
	if subfiles == nil {
		return path, false
	}
	file := strings.Trim(path, "/")
	if _, exists := subfiles[file]; !exists {
		for _, tf := range tryfiles {
			// If we encounter an absolute-path tryfile, and it exists, we stop
			// searching.
			_, exists = subfiles[strings.Trim(tf, "/")]
			if strings.HasPrefix(tf, "/") && exists {
				return tf, true
			}
			// Assume the request is for a directory and check if a
			// tryfile matches.
			potentialFilename := strings.Trim(strings.TrimSuffix(file, "/")+skymodules.EnsurePrefix(tf, "/"), "/")
			if _, exists = subfiles[potentialFilename]; exists {
				return skymodules.EnsurePrefix(potentialFilename, "/"), true
			}
		}
	}
	return path, false
}
