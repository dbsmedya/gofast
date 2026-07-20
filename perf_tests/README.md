# GoFast Performance Testing Framework

This directory contains the performance testing suite for GoFast. It uses the actual gofast CLI binary to test parsing performance across different slow log datasets.

## Overview

The performance testing framework:
- Discovers test datasets from subdirectories in your data directory
- Runs the actual `gofast-cli` binary on each dataset
- Measures real-world performance metrics
- Generates JSON, HTML (with charts), and text reports

## Prerequisites

1. Build the gofast-cli binary:
```bash
cd /path/to/gofast
make build-cli
```

2. Have slow log test data organized in directories

3. (Optional) Python 3 with jinja2 for HTML report generation:
```bash
pip3 install jinja2
```

## Usage

### Basic Usage

```bash
cd /path/to/gofast/perf_tests

# Run tests with default settings
./run_perf_tests.sh --data-dir=/app_data/slow_logs
```

### Expected Directory Structure

```
/app_data/slow_logs/
├── dataset_small/           # Small dataset (~10MB)
│   ├── slow.log
│   └── slow.log.1
├── dataset_medium/          # Medium dataset (~100MB)
│   └── mysql-slow.log
├── dataset_large/           # Large dataset (~1GB)
│   ├── slow.log
│   ├── slow.log.1
│   └── slow.log.2
└── production_sample/       # Production sample
    └── slow-queries.log
```

### Command-Line Options

```bash
./run_perf_tests.sh [OPTIONS]

Options:
    -d, --data-dir DIR      Test data directory (default: ./test_data)
    -o, --output DIR        Results output directory (default: ./results)
    -c, --cli PATH          Path to gofast-cli binary (default: ../bin/gofast-cli)
    -i, --iterations N      Number of iterations per dataset (default: 1)
    -h, --help              Show help message
```

### Environment Variables

Instead of command-line flags, you can use environment variables:

```bash
export DATA_DIR=/app_data/slow_logs
export RESULTS_DIR=./my_results
export CLI_BIN=/usr/local/bin/gofast-cli
export ITERATIONS=3

./run_perf_tests.sh
```

### Example Commands

#### Test with specific data directory
```bash
./run_perf_tests.sh --data-dir=/app_data/slow_logs
```

#### Run multiple iterations for averaging
```bash
./run_perf_tests.sh --data-dir=/app_data/slow_logs --iterations=3
```

#### Use custom CLI binary
```bash
./run_perf_tests.sh --cli=/usr/local/bin/gofast-cli --data-dir=/data/logs
```

#### Specify custom output directory
```bash
./run_perf_tests.sh --data-dir=/data/logs --output=/tmp/perf_results
```

## Setting Up Test Data

### Using Real Production Data

1. Create the test data directory:
```bash
mkdir -p /app_data/slow_logs/production_sample
```

2. Copy your slow logs:
```bash
cp /var/log/mysql/slow.log /app_data/slow_logs/production_sample/
```

3. (Optional) Anonymize sensitive data:
```bash
# Remove IP addresses
find /app_data/slow_logs -name "*.log" -exec \
  sed -i 's/[0-9]\{1,3\}\.[0-9]\{1,3\}\.[0-9]\{1,3\}\.[0-9]\{1,3\}/XXX.XXX.XXX.XXX/g' {} \;
```

### Creating Synthetic Test Data

You can use existing tools or create synthetic slow logs for testing:

```bash
# Example: Use mysqlslap to generate slow queries
mysqlslap --host=localhost --user=root --password \
  --concurrency=50 --iterations=100 \
  --query="SELECT SLEEP(RAND()*2)" \
  --debug-info --verbose

# Then copy the slow log
sudo cp /var/lib/mysql/slow.log /app_data/slow_logs/synthetic_test/
```

Or use a simple generator script:
```bash
#!/bin/bash
# generate_slow_logs.sh - Simple slow log generator

OUTPUT_DIR="${1:-./test_data}"
SIZE_MB="${2:-10}"

mkdir -p "$OUTPUT_DIR"

# Generate entries until target size reached
> "$OUTPUT_DIR/slow.log"

while [ $(stat -f%z "$OUTPUT_DIR/slow.log" 2>/dev/null || stat -c%s "$OUTPUT_DIR/slow.log" 2>/dev/null | head -1) -lt $((SIZE_MB * 1024 * 1024)) ]; do
    cat >> "$OUTPUT_DIR/slow.log" << 'EOF'
# Time: 2024-01-15T10:30:45.123456Z
# User@Host: app_user[app_user] @  [10.0.1.100]  Id:   123
# Query_time: 2.500234  Lock_time: 0.000123 Rows_sent: 100  Rows_examined: 50000
SET timestamp=1705314645;
SELECT * FROM users WHERE status = 'active' AND created_at > '2024-01-01' ORDER BY id DESC LIMIT 100;
EOF
done

echo "Generated $OUTPUT_DIR/slow.log ($(du -h "$OUTPUT_DIR/slow.log" | cut -f1))"
```

## Generated Reports

After running tests, three reports are generated in the output directory:

### 1. `performance_report.html`
Interactive HTML report with:
- Summary metrics cards
- Bar charts for throughput and entries/sec
- Scatter plot for size vs duration
- Detailed results table

### 2. `perf_results.json`
Machine-readable JSON with all test data:
```json
[
  {
    "dataset": "dataset_small",
    "iteration": 1,
    "files_processed": 2,
    "size_bytes": 10485760,
    "size_mb": 10.0,
    "entries_parsed": 5000,
    "duration_sec": 0.523,
    "throughput_mbps": 19.12,
    "entries_per_sec": 9560,
    "errors": [],
    "timestamp": "2026-02-28T15:30:00Z"
  }
]
```

### 3. `performance_report.txt`
Plain text report suitable for:
- Email sharing
- CI/CD logs
- Documentation

## Metrics Collected

For each dataset, the following metrics are collected:

| Metric | Description | Unit |
|--------|-------------|------|
| Files Processed | Number of log files | count |
| Size | Total size of log files | MB |
| Entries Parsed | Number of slow log entries | count |
| Duration | Time to parse all files | seconds |
| Throughput | Processing speed | MB/s |
| Entries/sec | Parsing rate | entries/s |

## Interpreting Results

### Throughput (MB/s)
- **Excellent**: > 50 MB/s
- **Good**: 20-50 MB/s
- **Acceptable**: 10-20 MB/s
- **Poor**: < 10 MB/s

### Entries/sec
- **Excellent**: > 20,000 entries/s
- **Good**: 10,000-20,000 entries/s
- **Acceptable**: 5,000-10,000 entries/s
- **Poor**: < 5,000 entries/s

### Linear Scaling
The size vs duration scatter plot should show roughly linear scaling. If duration increases faster than size, there may be memory pressure or I/O bottlenecks.

## CI/CD Integration

### GitHub Actions Example

```yaml
name: Performance Tests

on:
  push:
    branches: [ main ]
  pull_request:
    branches: [ main ]

jobs:
  perf-test:
    runs-on: ubuntu-latest
    steps:
    - uses: actions/checkout@v3
    
    - name: Set up Go
      uses: actions/setup-go@v3
      with:
        go-version: 1.21
    
    - name: Build CLI
      run: make build-cli
    
    - name: Download test data
      run: |
        mkdir -p /app_data/slow_logs
        aws s3 sync s3://my-bucket/test-data/slow_logs /app_data/slow_logs
    
    - name: Run performance tests
      run: |
        cd perf_tests
        ./run_perf_tests.sh \
          --data-dir=/app_data/slow_logs \
          --output=./results \
          --iterations=3
    
    - name: Upload results
      uses: actions/upload-artifact@v3
      with:
        name: performance-report
        path: perf_tests/results/
        
    - name: Comment on PR
      uses: actions/github-script@v6
      with:
        script: |
          const fs = require('fs');
          const summary = fs.readFileSync('perf_tests/results/performance_report.txt', 'utf8');
          github.rest.issues.createComment({
            issue_number: context.issue.number,
            owner: context.repo.owner,
            repo: context.repo.repo,
            body: '## Performance Test Results\n\n```\n' + summary + '\n```'
          });
```

## Troubleshooting

### "No datasets found"
```bash
# Check directory structure
ls -la $DATA_DIR

# Ensure .log files exist
find $DATA_DIR -name "*.log" -type f
```

### "gofast-cli not found"
```bash
# Build the CLI
make build-cli

# Or specify path
./run_perf_tests.sh --cli=/path/to/gofast-cli
```

### Slow performance
- Use SSD for test data
- Ensure sufficient RAM (at least 4GB)
- Check CPU usage during test
- Close other resource-intensive applications

### Permission denied
```bash
# Make script executable
chmod +x perf_tests/run_perf_tests.sh
```

## Advanced Usage

### Custom Test Scenarios

You can modify the test script for specific scenarios:

1. **Memory-constrained testing**: Use `ulimit` to limit memory
2. **Network filesystem testing**: Place data on NFS/SMB mount
3. **Concurrent testing**: Run multiple instances simultaneously

### Performance Regression Testing

Compare results between versions:

```bash
# Test baseline version
git checkout v1.0.0
make build-cli
./perf_tests/run_perf_tests.sh --output=./baseline_results

# Test current version
git checkout main
make build-cli
./perf_tests/run_perf_tests.sh --output=./current_results

# Compare
python3 compare_results.py baseline_results/perf_results.json current_results/perf_results.json
```

---

*For more information, see the main project README.md*
