#!/bin/bash
#
# GoFast Performance Testing Suite
# Runs performance tests on slow log datasets using the gofast CLI
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
DATA_DIR="${DATA_DIR:-$SCRIPT_DIR/test_data}"
RESULTS_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results}"
CLI_BIN="${CLI_BIN:-$PROJECT_DIR/bin/gofast-cli}"
ITERATIONS="${ITERATIONS:-1}"

# Print banner
echo ""
echo -e "${BLUE}╔══════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║       GoFast Performance Testing Suite                   ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""

# Help function
show_help() {
    cat << EOF
Usage: $0 [OPTIONS]

Options:
    -d, --data-dir DIR      Test data directory (default: $DATA_DIR)
    -o, --output DIR        Results output directory (default: $RESULTS_DIR)
    -c, --cli PATH          Path to gofast-cli binary (default: $CLI_BIN)
    -i, --iterations N      Number of iterations per dataset (default: $ITERATIONS)
    -h, --help              Show this help message

Environment Variables:
    DATA_DIR                Test data directory
    RESULTS_DIR             Results output directory
    CLI_BIN                 Path to gofast-cli binary
    ITERATIONS              Number of iterations per dataset

Example:
    $0 -d /app_data/slow_logs -o ./results -i 3
    DATA_DIR=/data/slow_logs $0

Expected Data Structure:
    DATA_DIR/
    ├── dataset_1/
    │   ├── slow.log
    │   └── slow.log.1
    ├── dataset_2/
    │   └── mysql-slow.log
    └── ...

EOF
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -d|--data-dir)
            DATA_DIR="$2"
            shift 2
            ;;
        --data-dir=*)
            DATA_DIR="${1#*=}"
            shift
            ;;
        -o|--output)
            RESULTS_DIR="$2"
            shift 2
            ;;
        --output=*)
            RESULTS_DIR="${1#*=}"
            shift
            ;;
        -c|--cli)
            CLI_BIN="$2"
            shift 2
            ;;
        --cli=*)
            CLI_BIN="${1#*=}"
            shift
            ;;
        -i|--iterations)
            ITERATIONS="$2"
            shift 2
            ;;
        --iterations=*)
            ITERATIONS="${1#*=}"
            shift
            ;;
        -h|--help)
            show_help
            exit 0
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            show_help
            exit 1
            ;;
    esac
done

# Verify CLI binary exists
if [[ ! -f "$CLI_BIN" ]]; then
    echo -e "${RED}Error: gofast-cli not found at $CLI_BIN${NC}"
    echo "Please build it first: make build-cli"
    exit 1
fi

# Verify data directory exists
if [[ ! -d "$DATA_DIR" ]]; then
    echo -e "${RED}Error: Data directory does not exist: $DATA_DIR${NC}"
    echo "Create it or set DATA_DIR environment variable"
    exit 1
fi

# Create results directory
mkdir -p "$RESULTS_DIR"

# Find all dataset directories
echo -e "${BLUE}Scanning for datasets in: $DATA_DIR${NC}"
echo ""

datasets=()
for dir in "$DATA_DIR"/*/; do
    if [[ -d "$dir" ]]; then
        # Check if directory contains log files (use -L to follow symlinks)
        if find -L "$dir" -type f \( -name "*slow*" -o -name "*.log*" \) 2>/dev/null | grep -q .; then
            dataset_name=$(basename "$dir")
            datasets+=("$dataset_name")
        fi
    fi
done

if [[ ${#datasets[@]} -eq 0 ]]; then
    echo -e "${RED}No datasets found in $DATA_DIR${NC}"
    echo "Expected subdirectories containing .log files"
    exit 1
fi

echo -e "${GREEN}Found ${#datasets[@]} dataset(s):${NC}"
for dataset in "${datasets[@]}"; do
    echo "  - $dataset"
done
echo ""

# Initialize results file
RESULTS_FILE="$RESULTS_DIR/perf_results.json"
echo "[]" > "$RESULTS_FILE"

# Summary statistics
total_datasets=${#datasets[@]}
total_files=0
total_entries=0
total_size=0
total_duration=0

# Run tests on each dataset
current=0
for dataset in "${datasets[@]}"; do
    ((current++))
    dataset_path="$DATA_DIR/$dataset"
    
    echo -e "${YELLOW}[$current/$total_datasets] Testing dataset: $dataset${NC}"
    
    # Calculate dataset size (look for files with 'slow' or 'log' in name)
    # Use -L to follow symlinks
    dataset_size=$(find -L "$dataset_path" -type f \( -name "*slow*" -o -name "*.log*" \) -exec stat -f%z {} + 2>/dev/null | awk '{sum+=$1} END {print sum}')
    if [[ -z "$dataset_size" ]]; then
        dataset_size=$(find -L "$dataset_path" -type f \( -name "*slow*" -o -name "*.log*" \) -exec stat -c%s {} + 2>/dev/null | awk '{sum+=$1} END {print sum}')
    fi
    dataset_size_mb=$(echo "scale=2; $dataset_size / 1024 / 1024" | bc)
    
    file_count=$(find -L "$dataset_path" -type f \( -name "*slow*" -o -name "*.log*" \) | wc -l)
    
    echo "  Files: $file_count | Size: ${dataset_size_mb}MB"
    
    # Run iterations
    for ((iter=1; iter<=ITERATIONS; iter++)); do
        if [[ $ITERATIONS -gt 1 ]]; then
            echo "  Iteration $iter/$ITERATIONS..."
        fi
        
        # Create temp database for this test
        test_db="$RESULTS_DIR/test_${dataset}_${iter}.duckdb"
        rm -f "$test_db" "$test_db.wal"
        
        # Record start time
        start_time=$(date +%s.%N)
        
        # Run parse
        set +e
        parse_output=$($CLI_BIN --duck-db-path "$test_db" --slow-log-dir "$dataset_path" parse -v 2>&1)
        parse_exit_code=$?
        set -e
        
        # Record end time
        end_time=$(date +%s.%N)
        duration=$(echo "$end_time - $start_time" | bc)
        
        # Extract metrics from output
        entries_parsed=$(echo "$parse_output" | grep "Entries parsed:" | awk '{print $3}')
        files_processed=$(echo "$parse_output" | grep "Files processed:" | awk '{print $3}')
        
        if [[ -z "$entries_parsed" ]]; then
            entries_parsed=0
        fi
        if [[ -z "$files_processed" ]]; then
            files_processed=$file_count
        fi
        
        # Calculate throughput
        if (( $(echo "$duration > 0" | bc -l) )); then
            throughput=$(echo "scale=2; $dataset_size_mb / $duration" | bc)
            entries_per_sec=$(echo "scale=0; $entries_parsed / $duration" | bc)
        else
            throughput=0
            entries_per_sec=0
        fi
        
        # Get stats from database
        stats_output=$($CLI_BIN --duck-db-path "$test_db" stats 2>&1 || true)
        
        # Check for errors
        errors="[]"
        if [[ $parse_exit_code -ne 0 ]]; then
            errors="[\"Parse failed with exit code $parse_exit_code\"]"
            echo -e "${RED}    ✗ Failed (exit code: $parse_exit_code)${NC}"
        else
            echo -e "${GREEN}    ✓ Parsed $entries_parsed entries in ${duration}s${NC}"
            echo "      Throughput: ${throughput}MB/s | Entries/sec: $entries_per_sec"
        fi
        
        # Clean up test database
        rm -f "$test_db" "$test_db.wal"
        
        # Build result JSON
        result=$(cat <<EOF
{
    "dataset": "$dataset",
    "iteration": $iter,
    "files_processed": $files_processed,
    "size_bytes": $dataset_size,
    "size_mb": $dataset_size_mb,
    "entries_parsed": $entries_parsed,
    "duration_sec": $duration,
    "throughput_mbps": $throughput,
    "entries_per_sec": $entries_per_sec,
    "errors": $errors,
    "timestamp": "$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
}
EOF
)
        
        # Append to results file
        python3 -c "
import json
with open('$RESULTS_FILE', 'r') as f:
    data = json.load(f)
data.append($result)
with open('$RESULTS_FILE', 'w') as f:
    json.dump(data, f, indent=2)
" 2>/dev/null || echo "$result" >> "$RESULTS_FILE.tmp"
        
        # Update totals
        total_files=$((total_files + files_processed))
        total_entries=$((total_entries + entries_parsed))
        total_duration=$(echo "$total_duration + $duration" | bc)
        
    done
    
    echo ""
done

# Calculate summary
if (( $(echo "$total_duration > 0" | bc -l) )); then
    avg_throughput=$(echo "scale=2; $total_size / $total_duration / 1024 / 1024" | bc)
    avg_entries_per_sec=$(echo "scale=0; $total_entries / $total_duration" | bc)
else
    avg_throughput=0
    avg_entries_per_sec=0
fi

# Generate summary report
echo -e "${BLUE}╔══════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║              Performance Test Summary                    ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════════════════════════╝${NC}"
echo ""
printf "%-20s %d\n" "Datasets Tested:" "$total_datasets"
printf "%-20s %d\n" "Total Files:" "$total_files"
printf "%-20s %.2f MB\n" "Total Size:" "$(echo "scale=2; $total_size / 1024 / 1024" | bc)"
printf "%-20s %d\n" "Total Entries:" "$total_entries"
printf "%-20s %.2f sec\n" "Total Duration:" "$total_duration"
echo ""
printf "%-20s %.2f MB/s\n" "Avg Throughput:" "$avg_throughput"
printf "%-20s %d\n" "Avg Entries/Sec:" "$avg_entries_per_sec"
echo ""

# Generate HTML report
echo -e "${BLUE}Generating reports...${NC}"
python3 "$SCRIPT_DIR/generate_report.py" "$RESULTS_FILE" "$RESULTS_DIR" 2>/dev/null || echo -e "${YELLOW}Note: Install Python with jinja2 for HTML report generation${NC}"

# Generate text report
REPORT_FILE="$RESULTS_DIR/performance_report.txt"
cat > "$REPORT_FILE" << EOF
GoFast Performance Test Report
==============================

Generated: $(date -u +"%Y-%m-%d %H:%M:%S UTC")
Data Directory: $DATA_DIR
CLI Binary: $CLI_BIN
Iterations per Dataset: $ITERATIONS

Summary
-------
Datasets Tested:     $total_datasets
Total Files:         $total_files
Total Size:          $(echo "scale=2; $total_size / 1024 / 1024" | bc) MB
Total Entries:       $total_entries
Total Duration:      $(printf "%.2f" $total_duration) sec
Avg Throughput:      $(printf "%.2f" $avg_throughput) MB/s
Avg Entries/Sec:     $avg_entries_per_sec

Detailed Results
----------------

EOF

# Add detailed results
python3 -c "
import json
with open('$RESULTS_FILE', 'r') as f:
    results = json.load(f)

for r in results:
    print(f\"Dataset: {r['dataset']} (Iteration {r['iteration']})\")
    print(f\"  Files:        {r['files_processed']}\")
    print(f\"  Size:         {r['size_mb']:.2f} MB\")
    print(f\"  Entries:      {r['entries_parsed']}\")
    print(f\"  Duration:     {r['duration_sec']:.3f} sec\")
    print(f\"  Throughput:   {r['throughput_mbps']:.2f} MB/s\")
    print(f\"  Entries/Sec:  {r['entries_per_sec']}\")
    if r['errors'] and len(r['errors']) > 0:
        print(f\"  Errors:       {len(r['errors'])}\")
    print()
" >> "$REPORT_FILE" 2>/dev/null || true

echo -e "${GREEN}✅ Performance testing complete!${NC}"
echo ""
echo "Results saved to:"
echo "  - $RESULTS_FILE"
echo "  - $REPORT_FILE"
echo "  - $RESULTS_DIR/performance_report.html (if Python available)"
echo ""
