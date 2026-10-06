//! TideMux's explicitly selected, ledger-derived usage source.
//! Unknowns and non-USD currencies cannot pass through the legacy USD/u64 core
//! report types. This adapter therefore owns its nullable source report schema.
use ccusage_core::{Result, cli_error};
use serde::{Deserialize, Serialize};
use std::{
    collections::{BTreeMap, BTreeSet},
    fs,
    io::{BufRead, BufReader, Read},
    path::Path,
};

#[derive(Clone, Debug, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Record {
    schema_version: u32,
    source: String,
    source_id: String,
    request_id: String,
    revision: u64,
    timestamp: String,
    session_group: Option<String>,
    provider: String,
    model: String,
    protocol: String,
    outcome: String,
    usage_source: String,
    input_tokens: Option<u64>,
    output_tokens: Option<u64>,
    cache_read_tokens: Option<u64>,
    cache_write_tokens: Option<u64>,
    estimated_cost: Option<f64>,
    currency: Option<String>,
    cost_source: String,
    price_source: String,
    price_version: Option<String>,
    supplier_amount: Option<f64>,
    supplier_statement_lines: u64,
    supplier_source: String,
}

fn hex_id(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

impl Record {
    fn validate(&self) -> Result<()> {
        if self.schema_version != 1
            || self.source != "tidemux"
            || !hex_id(&self.source_id)
            || self.request_id.is_empty()
            || self.request_id.len() > 256
            || self.session_group.as_ref().is_some_and(|s| !hex_id(s))
            || self
                .currency
                .as_ref()
                .is_some_and(|s| s.len() != 3 || !s.bytes().all(|b| b.is_ascii_uppercase()))
            || self
                .estimated_cost
                .is_some_and(|n| !n.is_finite() || n < 0.0)
            || self
                .supplier_amount
                .is_some_and(|n| !n.is_finite() || n < 0.0)
            || self.supplier_amount.is_some() != (self.supplier_statement_lines > 0)
            || (self.supplier_source == "matched_supplier_statement")
                != self.supplier_amount.is_some()
            || !["unknown", "matched_supplier_statement"].contains(&self.supplier_source.as_str())
            || (self.supplier_statement_lines > 0
                && (self.revision == 0 || self.currency.is_none()))
            || !["provider", "local_estimate", "unknown"].contains(&self.usage_source.as_str())
            || ![
                "unknown",
                "estimated_from_provider_usage",
                "local_content_estimate",
                "historical_estimate",
            ]
            .contains(&self.cost_source.as_str())
        {
            return Err(cli_error("invalid TideMux usage record contract"));
        }
        self.timestamp
            .parse::<jiff::Timestamp>()
            .map_err(|_| cli_error("invalid TideMux timestamp"))?;
        Ok(())
    }
}

/// Complete sums remain null if even one request is unknown. The known subtotal
/// and coverage are separate, so zero is never substituted for unknown usage.
#[derive(Default, Debug, Serialize)]
pub struct Coverage {
    pub total: Option<f64>,
    pub known_subtotal: Option<f64>,
    pub known_requests: u64,
    pub unknown_requests: u64,
}
impl Coverage {
    fn add(&mut self, amount: Option<f64>) -> Result<()> {
        match amount {
            Some(n) => {
                let subtotal = self.known_subtotal.unwrap_or(0.0) + n;
                if !subtotal.is_finite() {
                    return Err(cli_error("TideMux summary overflow"));
                }
                self.known_subtotal = Some(subtotal);
                self.known_requests += 1;
            }
            None => self.unknown_requests += 1,
        }
        self.total = if self.unknown_requests == 0 {
            self.known_subtotal
        } else {
            None
        };
        Ok(())
    }
}

#[derive(Default, Debug, Serialize)]
pub struct TokenCoverage {
    pub total: Option<u64>,
    pub known_subtotal: Option<u64>,
    pub known_requests: u64,
    pub unknown_requests: u64,
}
impl TokenCoverage {
    fn add(&mut self, amount: Option<u64>) -> Result<()> {
        match amount {
            Some(n) => {
                self.known_subtotal = Some(
                    self.known_subtotal
                        .unwrap_or(0)
                        .checked_add(n)
                        .ok_or_else(|| cli_error("TideMux token summary overflow"))?,
                );
                self.known_requests += 1;
            }
            None => self.unknown_requests += 1,
        }
        self.total = if self.unknown_requests == 0 {
            self.known_subtotal
        } else {
            None
        };
        Ok(())
    }
}

#[derive(Default, Debug, Serialize)]
pub struct Summary {
    pub group: Option<String>,
    pub currency: Option<String>,
    pub requests: u64,
    pub input_tokens: TokenCoverage,
    pub output_tokens: TokenCoverage,
    pub cache_read_tokens: TokenCoverage,
    pub cache_write_tokens: TokenCoverage,
    pub estimated_cost: Coverage,
    pub matched_supplier_amount: Coverage,
    pub supplier_statement_lines: u64,
    pub providers: BTreeSet<String>,
    pub models: BTreeSet<String>,
    pub outcomes: BTreeMap<String, u64>,
    pub usage_sources: BTreeMap<String, u64>,
    pub cost_sources: BTreeMap<String, u64>,
    pub supplier_sources: BTreeMap<String, u64>,
    pub price_sources: BTreeSet<String>,
    pub price_versions: BTreeSet<String>,
}

#[derive(Debug, Serialize)]
pub struct Report {
    pub schema_version: u32,
    pub source: &'static str,
    pub grouping: String,
    pub timezone: &'static str,
    pub retained_records: usize,
    pub replayed_records: usize,
    pub incomplete_tail_ignored: bool,
    pub rows: Vec<Summary>,
}

fn names(path: &Path) -> Result<Vec<std::path::PathBuf>> {
    let mut names = Vec::new();
    for entry in fs::read_dir(path)? {
        let entry = entry?;
        let name = entry.file_name();
        let Some(name) = name.to_str() else { continue };
        if name.len() == 32
            && name.starts_with("usage-")
            && name.ends_with(".jsonl")
            && name.as_bytes()[6..26].iter().all(|b| b.is_ascii_digit())
        {
            if !entry.file_type()?.is_file() {
                return Err(cli_error("TideMux input must be regular files"));
            }
            names.push(entry.path());
        }
    }
    names.sort();
    Ok(names)
}

pub fn report(path: &Path, grouping: &str) -> Result<Report> {
    if !["session", "daily", "monthly", "aggregate"].contains(&grouping) {
        return Err(cli_error(
            "TideMux reports: session, daily, monthly, aggregate",
        ));
    }
    let mut latest = BTreeMap::<(String, String), Record>::new();
    let mut replayed_records = 0;
    let mut incomplete_tail_ignored = false;
    for path in names(path)? {
        let mut reader = BufReader::new(fs::File::open(path)?);
        let mut line = Vec::new();
        loop {
            line.clear();
            // Bound memory even if an input was corrupted or replaced.
            let bytes = (&mut reader).take(65_538).read_until(b'\n', &mut line)?;
            if bytes == 0 {
                break;
            }
            if bytes > 65_537 {
                return Err(cli_error("TideMux usage line too large"));
            }
            if line.last() != Some(&b'\n') {
                incomplete_tail_ignored = true;
                break;
            }
            let record: Record = serde_json::from_slice(&line)
                .map_err(|_| cli_error("invalid complete TideMux JSONL record"))?;
            record.validate()?;
            let key = (record.source_id.clone(), record.request_id.clone());
            if let Some(previous) = latest.get(&key) {
                replayed_records += 1;
                if previous.revision > record.revision {
                    continue;
                }
                if previous.revision == record.revision {
                    if previous != &record {
                        return Err(cli_error("conflicting TideMux record revision"));
                    }
                    continue;
                }
                // Immutable request fields must never be changed by a bill revision.
                let mut baseline = record.clone();
                baseline.revision = previous.revision;
                baseline.supplier_amount = previous.supplier_amount;
                baseline.supplier_statement_lines = previous.supplier_statement_lines;
                baseline.supplier_source = previous.supplier_source.clone();
                if baseline != *previous {
                    return Err(cli_error("TideMux revision changed immutable request"));
                }
            }
            latest.insert(key, record);
        }
    }
    let mut rows = BTreeMap::<(Option<String>, Option<String>), Summary>::new();
    for record in latest.values() {
        let timestamp = record
            .timestamp
            .parse::<jiff::Timestamp>()
            .map_err(|_| cli_error("invalid TideMux timestamp"))?;
        let utc = timestamp
            .to_zoned(jiff::tz::TimeZone::UTC)
            .date()
            .to_string();
        let group = match grouping {
            "session" => record.session_group.clone(),
            "daily" => Some(utc),
            "monthly" => Some(utc[..7].to_owned()),
            _ => None,
        };
        let row = rows
            .entry((group.clone(), record.currency.clone()))
            .or_insert_with(|| Summary {
                group,
                currency: record.currency.clone(),
                ..Summary::default()
            });
        row.requests += 1;
        row.input_tokens.add(record.input_tokens)?;
        row.output_tokens.add(record.output_tokens)?;
        row.cache_read_tokens.add(record.cache_read_tokens)?;
        row.cache_write_tokens.add(record.cache_write_tokens)?;
        row.estimated_cost.add(record.estimated_cost)?;
        row.matched_supplier_amount.add(record.supplier_amount)?;
        row.supplier_statement_lines += record.supplier_statement_lines;
        row.providers.insert(record.provider.clone());
        row.models.insert(record.model.clone());
        *row.outcomes.entry(record.outcome.clone()).or_default() += 1;
        *row.usage_sources
            .entry(record.usage_source.clone())
            .or_default() += 1;
        *row.cost_sources
            .entry(record.cost_source.clone())
            .or_default() += 1;
        *row.supplier_sources
            .entry(record.supplier_source.clone())
            .or_default() += 1;
        row.price_sources.insert(record.price_source.clone());
        if let Some(version) = &record.price_version {
            row.price_versions.insert(version.clone());
        }
    }
    Ok(Report {
        schema_version: 1,
        source: "tidemux",
        grouping: grouping.to_owned(),
        timezone: "UTC",
        retained_records: latest.len(),
        replayed_records,
        incomplete_tail_ignored,
        rows: rows.into_values().collect(),
    })
}

pub fn run(args: Vec<String>) -> Result<()> {
    if args.is_empty() || args.iter().any(|a| a == "--help" || a == "-h") {
        println!(
            "ccusage tidemux <session|daily|monthly|aggregate> --path <dedicated-usage-directory> [--json]\nVersion: tidemux-adapter-v1; pinned upstream e12b7dd9c14494808057df1897d07edc999081eb\nReports cover retained TideMux records only. Currency rows are separate; null remains unknown.\nEstimated cost and matched supplier amount are independent columns. This source is explicitly selected to avoid counting client logs twice."
        );
        return Ok(());
    }
    let mut path = None;
    let mut json = false;
    let mut index = 1;
    while index < args.len() {
        match args[index].as_str() {
            "--path" if index + 1 < args.len() => {
                path = Some(args[index + 1].clone());
                index += 2;
            }
            "--json" => {
                json = true;
                index += 1;
            }
            _ => return Err(cli_error("unsupported TideMux report argument; use --help")),
        }
    }
    let report = report(
        Path::new(
            path.as_deref()
                .ok_or_else(|| cli_error("TideMux requires --path"))?,
        ),
        &args[0],
    )?;
    if json {
        println!("{}", serde_json::to_string(&report)?);
    } else {
        println!("{}", serde_json::to_string_pretty(&report)?);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn record(id: &str, currency: Option<&str>) -> serde_json::Value {
        json!({"schema_version":1,"source":"tidemux","source_id":"a".repeat(64),"request_id":id,"revision":0,
            "timestamp":"2026-10-06T12:00:00Z","session_group":"b".repeat(64),"provider":"fixture","model":"fixture",
            "protocol":"openai","outcome":"ok","usage_source":"provider","input_tokens":3,"output_tokens":2,
            "cache_read_tokens":null,"cache_write_tokens":0,"estimated_cost":1.0,"currency":currency,
            "cost_source":"estimated_from_provider_usage","price_source":"audit_price_snapshot","price_version":"1",
            "supplier_amount":null,"supplier_statement_lines":0,"supplier_source":"unknown"})
    }

    fn fixture(lines: &[serde_json::Value], tail: &str) -> std::path::PathBuf {
        let directory = std::env::temp_dir().join(format!(
            "ccusage-tidemux-test-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir(&directory).unwrap();
        let mut data = String::new();
        for row in lines {
            data.push_str(&row.to_string());
            data.push('\n');
        }
        data.push_str(tail);
        fs::write(directory.join("usage-00000000000000000001.jsonl"), data).unwrap();
        directory
    }

    #[test]
    fn unknown_is_not_zero_and_supplier_does_not_replace_estimate() {
        let mut value = Coverage::default();
        value.add(Some(0.0)).unwrap();
        value.add(None).unwrap();
        assert_eq!(value.total, None);
        assert_eq!(value.known_subtotal, Some(0.0));
        assert_eq!(value.unknown_requests, 1);
    }

    #[test]
    fn source_report_deduplicates_revisions_partitions_currency_and_retains_unknowns() {
        let usd = record("usd", Some("USD"));
        let mut unknown = record("unknown", Some("USD"));
        unknown["input_tokens"] = json!(null);
        unknown["estimated_cost"] = json!(null);
        unknown["cost_source"] = json!("unknown");
        unknown["usage_source"] = json!("unknown");
        let mut eur = record("eur", Some("EUR"));
        eur["session_group"] = json!(null);
        eur["input_tokens"] = json!(9_007_199_254_740_993_u64);
        let mut revision = usd.clone();
        revision["revision"] = json!(2);
        revision["supplier_amount"] = json!(4.0);
        revision["supplier_statement_lines"] = json!(1);
        revision["supplier_source"] = json!("matched_supplier_statement");
        let directory = fixture(
            &[usd.clone(), unknown, eur, revision.clone(), revision, usd],
            "{\"half\":",
        );
        let summary = report(&directory, "aggregate").unwrap();
        assert_eq!(summary.retained_records, 3);
        assert_eq!(summary.replayed_records, 3);
        assert!(summary.incomplete_tail_ignored);
        let usd = summary
            .rows
            .iter()
            .find(|r| r.currency.as_deref() == Some("USD"))
            .unwrap();
        assert_eq!(usd.requests, 2);
        assert_eq!(usd.input_tokens.total, None);
        assert_eq!(usd.input_tokens.known_subtotal, Some(3));
        assert_eq!(usd.estimated_cost.total, None);
        assert_eq!(usd.estimated_cost.known_subtotal, Some(1.0));
        assert_eq!(usd.matched_supplier_amount.known_subtotal, Some(4.0));
        assert_eq!(usd.supplier_statement_lines, 1);
        let eur = summary
            .rows
            .iter()
            .find(|r| r.currency.as_deref() == Some("EUR"))
            .unwrap();
        assert_eq!(eur.input_tokens.total, Some(9_007_199_254_740_993));
        let sessions = report(&directory, "session").unwrap();
        assert!(
            sessions
                .rows
                .iter()
                .any(|r| r.group.is_none() && r.currency.as_deref() == Some("EUR"))
        );
        fs::remove_dir_all(directory).unwrap();
    }

    #[test]
    fn conflicting_revision_and_malformed_completed_line_fail_without_payload() {
        let original = record("one", Some("USD"));
        let mut conflict = original.clone();
        conflict["input_tokens"] = json!(100);
        let directory = fixture(&[original, conflict], "");
        assert!(report(&directory, "daily").is_err());
        fs::remove_dir_all(directory).unwrap();
        let directory = fixture(&[], "private-secret-malformed\n");
        let error = format!("{:?}", report(&directory, "daily").unwrap_err());
        assert!(!error.contains("private-secret"));
        fs::remove_dir_all(directory).unwrap();
    }
}
