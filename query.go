package sqlds

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana-plugin-sdk-go/data/sqlutil"
)

// FormatQueryOption defines how the user has chosen to represent the data
type FormatQueryOption uint32

const (
	// FormatOptionTimeSeries formats the query results as a timeseries using "WideToLong"
	FormatOptionTimeSeries FormatQueryOption = iota
	// FormatOptionTable formats the query results as a table using "LongToWide"
	FormatOptionTable
	// FormatOptionLogs sets the preferred visualization to logs
	FormatOptionLogs
	// FormatOptionsTrace sets the preferred visualization to trace
	FormatOptionTrace
)

// Query is the model that represents the query that users submit from the panel / queryeditor.
// For the sake of backwards compatibility, when making changes to this type, ensure that changes are
// only additive.
type Query struct {
	RawSQL         string            `json:"rawSql"`
	Format         FormatQueryOption `json:"format"`
	ConnectionArgs json.RawMessage   `json:"connectionArgs"`

	RefID         string            `json:"-"`
	Interval      time.Duration     `json:"-"`
	TimeRange     backend.TimeRange `json:"-"`
	MaxDataPoints int64             `json:"-"`
	FillMissing   *data.FillMissing `json:"fillMode,omitempty"`

	// Macros
	Schema string `json:"schema,omitempty"`
	Table  string `json:"table,omitempty"`
	Column string `json:"column,omitempty"`
}

// WithSQL copies the Query, but with a different RawSQL value.
// This is mostly useful in the Interpolate function, where the RawSQL value is modified in a loop
func (q *Query) WithSQL(query string) *Query {
	return &Query{
		RawSQL:         query,
		ConnectionArgs: q.ConnectionArgs,
		RefID:          q.RefID,
		Interval:       q.Interval,
		TimeRange:      q.TimeRange,
		MaxDataPoints:  q.MaxDataPoints,
		FillMissing:    q.FillMissing,
		Schema:         q.Schema,
		Table:          q.Table,
		Column:         q.Column,
	}
}

// GetQuery returns a Query object given a backend.DataQuery using json.Unmarshal
func GetQuery(query backend.DataQuery, headers http.Header, setHeaders bool) (*Query, error) {
	model := &Query{}

	if err := json.Unmarshal(query.JSON, &model); err != nil {
		return nil, PluginError(fmt.Errorf("%w: %v", ErrorJSON, err))
	}

	if setHeaders {
		applyHeaders(model, headers)
	}

	// Copy directly from the well typed query
	return &Query{
		RawSQL:         model.RawSQL,
		Format:         model.Format,
		ConnectionArgs: model.ConnectionArgs,
		RefID:          query.RefID,
		Interval:       query.Interval,
		TimeRange:      query.TimeRange,
		MaxDataPoints:  query.MaxDataPoints,
		FillMissing:    model.FillMissing,
		Schema:         model.Schema,
		Table:          model.Table,
		Column:         model.Column,
	}, nil
}

// getErrorFrameFromQuery returns a error frames with empty data and meta fields
func getErrorFrameFromQuery(query *Query) data.Frames {
	frames := data.Frames{}
	frame := data.NewFrame(query.RefID)
	frame.Meta = &data.FrameMeta{
		ExecutedQueryString: query.RawSQL,
	}
	frames = append(frames, frame)
	return frames
}

// QueryDB sends the query to the connection and converts the rows to a dataframe.
func QueryDB(ctx context.Context, db Connection, converterGenerator *sqlutil.ConverterGenerator, converters []sqlutil.Converter, fillMode *data.FillMissing, query *Query, args ...interface{}) (data.Frames, error) {
	// Query the rows from the database
	rows, err := db.QueryContext(ctx, query.RawSQL, args...)
	if err != nil {
		errType := ErrorQuery
		if errors.Is(err, context.Canceled) {
			errType = context.Canceled
		}
		err := DownstreamError(fmt.Errorf("%w: %s", errType, err.Error()))
		return getErrorFrameFromQuery(query), err
	}

	// Check for an error response
	if err := rows.Err(); err != nil {
		if err == sql.ErrNoRows {
			// Should we even response with an error here?
			// The panel will simply show "no data"
			err := DownstreamError(fmt.Errorf("%s: %w", "No results from query", err))
			return getErrorFrameFromQuery(query), err
		}
		err := DownstreamError(fmt.Errorf("%s: %w", "Error response from database", err))
		return getErrorFrameFromQuery(query), err
	}

	defer func() {
		if err := rows.Close(); err != nil {
			backend.Logger.Error(err.Error())
		}
	}()

	// Convert the response to frames
	res, err := getFrames(rows, -1, converterGenerator, converters, fillMode, query)
	if err != nil {
		err := PluginError(fmt.Errorf("%w: %s", err, "Could not process SQL results"))
		return getErrorFrameFromQuery(query), err
	}

	return res, nil
}

func getFrames(rows *sql.Rows, limit int64, converterGenerator *sqlutil.ConverterGenerator, converters []sqlutil.Converter, fillMode *data.FillMissing, query *Query) (data.Frames, error) {
	frame, err := sqlutil.FrameFromRows(rows, limit, converterGenerator, converters...)
	if err != nil {
		return nil, err
	}
	frame.Name = query.RefID
	if frame.Meta == nil {
		frame.Meta = &data.FrameMeta{}
	}

	frame.Meta.ExecutedQueryString = query.RawSQL
	frame.Meta.PreferredVisualization = data.VisTypeGraph

	if query.Format == FormatOptionTable {
		frame.Meta.PreferredVisualization = data.VisTypeTable
		return data.Frames{frame}, nil
	}

	if query.Format == FormatOptionLogs {
		frame.Meta.PreferredVisualization = data.VisTypeLogs
		return data.Frames{frame}, nil
	}

	if query.Format == FormatOptionTrace {
		frame.Meta.PreferredVisualization = data.VisTypeTrace
		return data.Frames{frame}, nil
	}

	count, err := frame.RowLen()

	if err != nil {
		return nil, err
	}

	if count == 0 {
		return nil, ErrorNoResults
	}

	if frame.TimeSeriesSchema().Type == data.TimeSeriesTypeLong {
		frame, err := data.LongToWide(frame, fillMode)
		if err != nil {
			return nil, err
		}
		return data.Frames{frame}, nil
	}

	return data.Frames{frame}, nil
}

func applyHeaders(query *Query, headers http.Header) *Query {
	var args map[string]interface{}
	if query.ConnectionArgs == nil {
		query.ConnectionArgs = []byte("{}")
	}
	err := json.Unmarshal(query.ConnectionArgs, &args)
	if err != nil {
		backend.Logger.Warn(fmt.Sprintf("Failed to apply headers: %s", err.Error()))
		return query
	}
	args[HeaderKey] = headers
	raw, err := json.Marshal(args)
	if err != nil {
		backend.Logger.Warn(fmt.Sprintf("Failed to apply headers: %s", err.Error()))
		return query
	}

	query.ConnectionArgs = raw

	return query
}
