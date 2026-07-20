#!/usr/bin/env python3
"""
Generate HTML performance report from JSON results.
Usage: python3 generate_report.py <results.json> <output_dir>
"""

import json
import sys
from datetime import datetime
from pathlib import Path

HTML_TEMPLATE = """<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>GoFast Performance Report</title>
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
    <style>
        * {{ box-sizing: border-box; }}
        body {{
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, sans-serif;
            margin: 0;
            padding: 20px;
            background: #f5f5f5;
        }}
        .container {{ max-width: 1400px; margin: 0 auto; }}
        h1 {{ color: #333; border-bottom: 2px solid #4CAF50; padding-bottom: 10px; }}
        h2 {{ color: #555; margin-top: 30px; }}
        .summary-grid {{
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 15px;
            margin: 20px 0;
        }}
        .metric-card {{
            background: white;
            padding: 20px;
            border-radius: 8px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }}
        .metric-value {{
            font-size: 2em;
            font-weight: bold;
            color: #4CAF50;
        }}
        .metric-label {{
            color: #666;
            font-size: 0.9em;
            margin-top: 5px;
        }}
        .chart-container {{
            background: white;
            padding: 20px;
            border-radius: 8px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
            margin: 20px 0;
            height: 400px;
        }}
        .results-table {{
            width: 100%;
            border-collapse: collapse;
            background: white;
            border-radius: 8px;
            overflow: hidden;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }}
        .results-table th, .results-table td {{
            padding: 12px;
            text-align: left;
            border-bottom: 1px solid #eee;
        }}
        .results-table th {{
            background: #4CAF50;
            color: white;
            font-weight: 600;
        }}
        .results-table tr:hover {{ background: #f9f9f9; }}
        .error {{ color: #f44336; }}
        .success {{ color: #4CAF50; }}
        .timestamp {{ color: #999; font-size: 0.9em; }}
    </style>
</head>
<body>
    <div class="container">
        <h1>🚀 GoFast Performance Report</h1>
        <p class="timestamp">Generated: {generated_at}</p>
        
        <h2>Summary</h2>
        <div class="summary-grid">
            <div class="metric-card">
                <div class="metric-value">{total_datasets}</div>
                <div class="metric-label">Datasets Tested</div>
            </div>
            <div class="metric-card">
                <div class="metric-value">{total_size_mb:.1f}</div>
                <div class="metric-label">Total Size (MB)</div>
            </div>
            <div class="metric-card">
                <div class="metric-value">{total_entries}</div>
                <div class="metric-label">Total Entries</div>
            </div>
            <div class="metric-card">
                <div class="metric-value">{avg_throughput:.1f}</div>
                <div class="metric-label">Avg Throughput (MB/s)</div>
            </div>
            <div class="metric-card">
                <div class="metric-value">{avg_entries_per_sec:.0f}</div>
                <div class="metric-label">Avg Entries/sec</div>
            </div>
            <div class="metric-card">
                <div class="metric-value">{total_duration:.1f}</div>
                <div class="metric-label">Total Duration (s)</div>
            </div>
        </div>

        <h2>Throughput by Dataset</h2>
        <div class="chart-container">
            <canvas id="throughputChart"></canvas>
        </div>

        <h2>Entries/sec by Dataset</h2>
        <div class="chart-container">
            <canvas id="entriesChart"></canvas>
        </div>

        <h2>Size vs Duration</h2>
        <div class="chart-container">
            <canvas id="scatterChart"></canvas>
        </div>

        <h2>Detailed Results</h2>
        <table class="results-table">
            <thead>
                <tr>
                    <th>Dataset</th>
                    <th>Iteration</th>
                    <th>Files</th>
                    <th>Size (MB)</th>
                    <th>Entries</th>
                    <th>Duration (s)</th>
                    <th>Throughput (MB/s)</th>
                    <th>Entries/sec</th>
                    <th>Status</th>
                </tr>
            </thead>
            <tbody>
                {table_rows}
            </tbody>
        </table>
    </div>

    <script>
        const datasets = {datasets_json};
        const throughputs = {throughputs_json};
        const entriesPerSec = {entries_per_sec_json};
        const sizes = {sizes_json};
        const durations = {durations_json};

        // Throughput Chart
        new Chart(document.getElementById('throughputChart'), {{
            type: 'bar',
            data: {{
                labels: datasets,
                datasets: [{{
                    label: 'Throughput (MB/s)',
                    data: throughputs,
                    backgroundColor: 'rgba(76, 175, 80, 0.6)',
                    borderColor: 'rgba(76, 175, 80, 1)',
                    borderWidth: 1
                }}]
            }},
            options: {{
                responsive: true,
                maintainAspectRatio: false,
                plugins: {{ legend: {{ display: false }} }},
                scales: {{ y: {{ beginAtZero: true, title: {{ display: true, text: 'MB/s' }} }} }}
            }}
        }});

        // Entries/sec Chart
        new Chart(document.getElementById('entriesChart'), {{
            type: 'bar',
            data: {{
                labels: datasets,
                datasets: [{{
                    label: 'Entries/sec',
                    data: entriesPerSec,
                    backgroundColor: 'rgba(33, 150, 243, 0.6)',
                    borderColor: 'rgba(33, 150, 243, 1)',
                    borderWidth: 1
                }}]
            }},
            options: {{
                responsive: true,
                maintainAspectRatio: false,
                plugins: {{ legend: {{ display: false }} }},
                scales: {{ y: {{ beginAtZero: true, title: {{ display: true, text: 'Entries/sec' }} }} }}
            }}
        }});

        // Scatter Chart
        new Chart(document.getElementById('scatterChart'), {{
            type: 'scatter',
            data: {{
                datasets: [{{
                    label: 'Datasets',
                    data: sizes.map((size, i) => ({{ x: size, y: durations[i] }})),
                    backgroundColor: 'rgba(255, 152, 0, 0.6)',
                    borderColor: 'rgba(255, 152, 0, 1)',
                    borderWidth: 1
                }}]
            }},
            options: {{
                responsive: true,
                maintainAspectRatio: false,
                plugins: {{ legend: {{ display: false }} }},
                scales: {{
                    x: {{ title: {{ display: true, text: 'Size (MB)' }} }},
                    y: {{ title: {{ display: true, text: 'Duration (sec)' }} }}
                }}
            }}
        }});
    </script>
</body>
</html>
"""

def generate_report(results_file, output_dir):
    # Load results
    with open(results_file, 'r') as f:
        results = json.load(f)
    
    if not results:
        print("No results to report")
        return
    
    # Calculate summary
    datasets = list(set(r['dataset'] for r in results))
    total_size = sum(r['size_bytes'] for r in results) / len(set((r['dataset'], r['iteration']) for r in results))
    total_entries = sum(r['entries_parsed'] for r in results)
    total_duration = sum(r['duration_sec'] for r in results)
    
    avg_throughput = sum(r['throughput_mbps'] for r in results) / len(results)
    avg_entries_per_sec = sum(r['entries_per_sec'] for r in results) / len(results)
    
    # Prepare chart data (average by dataset)
    dataset_stats = {}
    for r in results:
        ds = r['dataset']
        if ds not in dataset_stats:
            dataset_stats[ds] = {'throughputs': [], 'entries_per_sec': [], 'size_mb': r['size_mb'], 'duration': []}
        dataset_stats[ds]['throughputs'].append(r['throughput_mbps'])
        dataset_stats[ds]['entries_per_sec'].append(r['entries_per_sec'])
        dataset_stats[ds]['duration'].append(r['duration_sec'])
    
    chart_datasets = list(dataset_stats.keys())
    chart_throughputs = [sum(dataset_stats[ds]['throughputs'])/len(dataset_stats[ds]['throughputs']) for ds in chart_datasets]
    chart_entries_per_sec = [sum(dataset_stats[ds]['entries_per_sec'])/len(dataset_stats[ds]['entries_per_sec']) for ds in chart_datasets]
    chart_sizes = [dataset_stats[ds]['size_mb'] for ds in chart_datasets]
    chart_durations = [sum(dataset_stats[ds]['duration'])/len(dataset_stats[ds]['duration']) for ds in chart_datasets]
    
    # Generate table rows
    table_rows = ""
    for r in results:
        has_errors = len(r.get('errors', [])) > 0
        status = '<span class="error">✗ Failed</span>' if has_errors else '<span class="success">✓ Success</span>'
        table_rows += f"""<tr>
            <td>{r['dataset']}</td>
            <td>{r['iteration']}</td>
            <td>{r['files_processed']}</td>
            <td>{r['size_mb']:.2f}</td>
            <td>{r['entries_parsed']}</td>
            <td>{r['duration_sec']:.3f}</td>
            <td>{r['throughput_mbps']:.2f}</td>
            <td>{r['entries_per_sec']:.0f}</td>
            <td>{status}</td>
        </tr>\n"""
    
    # Generate HTML
    html = HTML_TEMPLATE.format(
        generated_at=datetime.utcnow().strftime('%Y-%m-%d %H:%M:%S UTC'),
        total_datasets=len(datasets),
        total_size_mb=total_size / 1024 / 1024,
        total_entries=total_entries,
        avg_throughput=avg_throughput,
        avg_entries_per_sec=avg_entries_per_sec,
        total_duration=total_duration,
        datasets_json=json.dumps(chart_datasets),
        throughputs_json=json.dumps(chart_throughputs),
        entries_per_sec_json=json.dumps(chart_entries_per_sec),
        sizes_json=json.dumps(chart_sizes),
        durations_json=json.dumps(chart_durations),
        table_rows=table_rows
    )
    
    # Write output
    output_path = Path(output_dir) / 'performance_report.html'
    output_path.write_text(html)
    print(f"HTML report generated: {output_path}")

if __name__ == '__main__':
    if len(sys.argv) != 3:
        print(f"Usage: {sys.argv[0]} <results.json> <output_dir>")
        sys.exit(1)
    
    generate_report(sys.argv[1], sys.argv[2])
