package ledger

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
)

// ExportBillingCSV streams local estimates and original supplier charges as
// separate rows. Empty numeric cells mean unknown; supplier_amount=0 is a known
// zero charge. Partial-period supplier lines are included with an explicit status
// so the downloaded bill never silently drops an overlapping supplier charge.
func (l *Ledger) ExportBillingCSV(ctx context.Context, w io.Writer, fromMS, toMS int64) error {
	if err := validatePeriod(fromMS, toMS); err != nil {
		return err
	}
	tx, err := l.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	writer := csv.NewWriter(w)
	if err = writer.Write([]string{"row_type", "request_id", "timestamp_ms", "period_start_ms", "period_end_ms", "currency", "model", "local_estimated", "supplier_amount", "status", "input_tokens", "output_tokens", "tokenizer_compared", "tokenizer_mismatch"}); err != nil {
		return err
	}
	where, args := periodWhere("timestamp_ms", fromMS, toMS)
	rows, err := tx.QueryContext(ctx, `SELECT request_id,timestamp_ms,currency,model,estimated_cost,input_tokens,output_tokens,statement_lines,tokenizer_compared,tokenizer_mismatch FROM reconciliation_requests`+where+` ORDER BY timestamp_ms,request_id`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, currency, model string
		var ts, lines, compared, mismatch int64
		var estimated sql.NullFloat64
		var input, output sql.NullInt64
		if err = rows.Scan(&id, &ts, &currency, &model, &estimated, &input, &output, &lines, &compared, &mismatch); err != nil {
			rows.Close()
			return err
		}
		status := "matched"
		if lines == 0 {
			status = "missing_statement"
		}
		if !estimated.Valid {
			status = "unknown_cost_" + status
		}
		row := []string{"request", id, strconv.FormatInt(ts, 10), "", "", currency, model, csvFloat(estimated), "", status, csvInt(input), csvInt(output), strconv.FormatInt(compared, 10), strconv.FormatInt(mismatch, 10)}
		if err = writer.Write(row); err != nil {
			rows.Close()
			return err
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	where, args = statementWhere(fromMS, toMS, true)
	rows, err = tx.QueryContext(ctx, `SELECT COALESCE(request_id,''),period_start_ms,period_end_ms,currency,model,amount,status FROM reconciliation_statements`+where+` ORDER BY period_start_ms,statement_id`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, currency, model, status string
		var start, end int64
		var amount float64
		if err = rows.Scan(&id, &start, &end, &currency, &model, &amount, &status); err != nil {
			rows.Close()
			return err
		}
		if (fromMS > 0 && start < fromMS) || (toMS > 0 && end > toMS) {
			status = "partial_period_" + status
		}
		row := []string{"supplier_statement", id, "", strconv.FormatInt(start, 10), strconv.FormatInt(end, 10), currency, model, "", strconv.FormatFloat(amount, 'g', -1, 64), status, "", "", "", ""}
		if err = writer.Write(row); err != nil {
			rows.Close()
			return err
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	writer.Flush()
	if err = writer.Error(); err != nil {
		return fmt.Errorf("write billing CSV: %w", err)
	}
	return tx.Commit()
}
func csvFloat(v sql.NullFloat64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatFloat(v.Float64, 'g', -1, 64)
}
func csvInt(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(v.Int64, 10)
}
