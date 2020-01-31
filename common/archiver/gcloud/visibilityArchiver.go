// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package gcloud

import (
	"context"
	"path/filepath"

	"github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common/archiver"
	"github.com/uber/cadence/common/archiver/gcloud/connector"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/service/config"
)

const (
	errEncodeVisibilityRecord = "failed to encode visibility record"
)

type (
	visibilityArchiver struct {
		container     *archiver.VisibilityBootstrapContainer
		gcloudStorage connector.Client
		queryParser   QueryParser
	}

	queryVisibilityToken struct {
		LastCloseTime int64
		LastRunID     string
	}

	visibilityRecord archiver.ArchiveVisibilityRequest

	queryVisibilityRequest struct {
		domainID      string
		pageSize      int
		nextPageToken []byte
		parsedQuery   *parsedQuery
	}
)

// NewVisibilityArchiver creates a new archiver.VisibilityArchiver based on filestore
func NewVisibilityArchiver(container *archiver.VisibilityBootstrapContainer, config *config.GstorageArchiver) (archiver.VisibilityArchiver, error) {
	storage, err := connector.NewClient(context.Background(), config)
	return &visibilityArchiver{
		container:     container,
		gcloudStorage: storage,
		queryParser:   NewQueryParser(),
	}, err
}

// Archive is used to archive one workflow visibility record.
// Check the Archive() method of the HistoryArchiver interface in Step 2 for parameters' meaning and requirements.
// The only difference is that the ArchiveOption parameter won't include an option for recording process.
// Please make sure your implementation is lossless. If any in-memory batching mechanism is used, then those batched records will be lost during server restarts.
// This method will be invoked when workflow closes. Note that because of conflict resolution, it is possible for a workflow to through the closing process multiple times, which means that this method can be invoked more than once after a workflow closes.
func (v *visibilityArchiver) Archive(ctx context.Context, URI archiver.URI, request *archiver.ArchiveVisibilityRequest, opts ...archiver.ArchiveOption) (err error) {
	featureCatalog := archiver.GetFeatureCatalog(opts...)
	defer func() {
		if err != nil && featureCatalog.NonRetriableError != nil {
			err = featureCatalog.NonRetriableError()
		}
	}()

	logger := archiver.TagLoggerWithArchiveVisibilityRequestAndURI(v.container.Logger, request, URI.String())

	if err := v.ValidateURI(URI); err != nil {
		logger.Error(archiver.ArchiveNonRetriableErrorMsg, tag.ArchivalArchiveFailReason(archiver.ErrReasonInvalidURI), tag.Error(err))
		return err
	}

	if err := archiver.ValidateVisibilityArchivalRequest(request); err != nil {
		logger.Error(archiver.ArchiveNonRetriableErrorMsg, tag.ArchivalArchiveFailReason(archiver.ErrReasonInvalidArchiveRequest), tag.Error(err))
		return err
	}

	encodedVisibilityRecord, err := encode(request)
	if err != nil {
		logger.Error(archiver.ArchiveNonRetriableErrorMsg, tag.ArchivalArchiveFailReason(errEncodeVisibilityRecord), tag.Error(err))
		return err
	}

	// The filename has the format: closeTimestamp_hash(runID).visibility
	// This format allows the archiver to sort all records without reading the file contents
	filename := constructVisibilityFilename(request.DomainID, request.WorkflowID, request.RunID, request.CloseTimestamp)
	if err := v.gcloudStorage.Upload(context.Background(), URI, filename, encodedVisibilityRecord); err != nil {
		logger.Error(archiver.ArchiveNonRetriableErrorMsg, tag.ArchivalArchiveFailReason(errWriteFile), tag.Error(err))
		return err
	}

	return nil
}

// Query is used to retrieve archived visibility records.
// Check the Get() method of the HistoryArchiver interface in Step 2 for parameters' meaning and requirements.
// The request includes a string field called query, which describes what kind of visibility records should be returned. For example, it can be some SQL-like syntax query string.
// Your implementation is responsible for parsing and validating the query, and also returning all visibility records that match the query.
// Currently the maximum context timeout passed into the method is 3 minutes, so it's ok if this method takes a long time to run.
func (v *visibilityArchiver) Query(ctx context.Context, URI archiver.URI, request *archiver.QueryVisibilityRequest) (*archiver.QueryVisibilityResponse, error) {
	if err := v.ValidateURI(URI); err != nil {
		return nil, &shared.BadRequestError{Message: archiver.ErrInvalidURI.Error()}
	}

	if err := archiver.ValidateQueryRequest(request); err != nil {
		return nil, &shared.BadRequestError{Message: archiver.ErrInvalidQueryVisibilityRequest.Error()}
	}

	parsedQuery, err := v.queryParser.Parse(request.Query)
	if err != nil {
		return nil, &shared.BadRequestError{Message: err.Error()}
	}

	if parsedQuery.emptyResult {
		return &archiver.QueryVisibilityResponse{}, nil
	}

	return v.query(ctx, URI, &queryVisibilityRequest{
		domainID:      request.DomainID,
		pageSize:      request.PageSize,
		nextPageToken: request.NextPageToken,
		parsedQuery:   parsedQuery,
	})
}

func (v *visibilityArchiver) query(ctx context.Context, URI archiver.URI, request *queryVisibilityRequest) (*archiver.QueryVisibilityResponse, error) {
	var token *queryVisibilityToken
	if request.nextPageToken != nil {
		var err error
		token, err = deserializeQueryVisibilityToken(request.nextPageToken)
		if err != nil {
			return nil, &shared.BadRequestError{Message: archiver.ErrNextPageTokenCorrupted.Error()}
		}
	}

	prefix := constructVisibilityFilenamePrefix(request.domainID, *request.parsedQuery.runID, *request.parsedQuery.workflowID)
	filenames, err := v.gcloudStorage.Query(ctx, URI, prefix)
	if err != nil {
		return nil, &shared.InternalServiceError{Message: err.Error()}
	}

	filenames, err = sortAndFilterFiles(filenames, token)
	if err != nil {
		return nil, &shared.InternalServiceError{Message: err.Error()}
	}
	if len(filenames) == 0 {
		return &archiver.QueryVisibilityResponse{}, nil
	}

	response := &archiver.QueryVisibilityResponse{}
	for idx, file := range filenames {
		encodedRecord, err := v.gcloudStorage.Get(ctx, URI, filepath.Base(file))
		if err != nil {
			return nil, &shared.InternalServiceError{Message: err.Error()}
		}

		record, err := decodeVisibilityRecord(encodedRecord)
		if err != nil {
			return nil, &shared.InternalServiceError{Message: err.Error()}
		}

		// if record.CloseTimestamp < request.parsedQuery.earliestCloseTime {
		// 	break
		// }

		if matchQuery(record, request.parsedQuery) {
			response.Executions = append(response.Executions, convertToExecutionInfo(record))
			if len(response.Executions) == request.pageSize {
				if idx != len(filenames) {
					newToken := &queryVisibilityToken{
						LastCloseTime: record.CloseTimestamp,
						LastRunID:     record.RunID,
					}
					encodedToken, err := serializeToken(newToken)
					if err != nil {
						return nil, &shared.InternalServiceError{Message: err.Error()}
					}
					response.NextPageToken = encodedToken
				}
				break
			}
		}
	}

	return response, nil
}

// ValidateURI is used to define what a valid URI for an implementation is.
func (v *visibilityArchiver) ValidateURI(URI archiver.URI) (err error) {

	if err = v.validateURI(URI); err == nil {
		_, err = v.gcloudStorage.Exist(context.Background(), URI, "")
	}

	return
}

func (v *visibilityArchiver) validateURI(URI archiver.URI) (err error) {
	if URI.Scheme() != URIScheme {
		return archiver.ErrURISchemeMismatch
	}

	if URI.Path() == "" || URI.Hostname() == "" {
		return archiver.ErrInvalidURI
	}

	return
}
