package app

const templates = `
{{define "head"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Renovate Scheduler</title>
<style>
:root { color-scheme: light dark; }
body { font-family: system-ui, sans-serif; margin: 0 auto; max-width: 72rem; padding: 1rem 1.5rem 3rem; line-height: 1.5; }
header { display: flex; align-items: center; gap: 1rem; border-bottom: 1px solid #8884; padding-bottom: .75rem; margin-bottom: 1.5rem; }
header h1 { font-size: 1.1rem; margin: 0; }
header nav { margin-left: auto; display: flex; gap: .75rem; align-items: center; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: .4rem .6rem; border-bottom: 1px solid #8883; vertical-align: top; }
code, pre { font-family: ui-monospace, monospace; }
pre { white-space: pre-wrap; word-break: break-all; background: #8881; padding: 1rem; border-radius: 6px; max-height: 70vh; overflow: auto; }
.button { font: inherit; padding: .2rem .6rem; cursor: pointer; }
.error { color: #b00; }
.muted { color: #888; }
.state-running { color: #06c; font-weight: 600; }
.state-queued { color: #a70; }
.ok { color: #070; }
.fail { color: #b00; }
input[type=search] { font: inherit; padding: .3rem .5rem; }
form { display: inline; }
</style>
</head>
<body>
<header>
<h1><a href="/" style="color:inherit;text-decoration:none">Renovate Scheduler</a></h1>
<nav>
{{if .Login}}<a href="/runs">Runs</a><span class="muted">{{.Login}}</span>
<form method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button class="button">Sign out</button></form>{{end}}
</nav>
</header>
{{end}}

{{define "foot"}}</body></html>{{end}}

{{define "login"}}{{template "head" .}}
<p>Sign in with your Forgejo account to choose which repositories Renovate should manage.</p>
<p><a href="/login">Sign in with Forgejo</a></p>
{{template "foot" .}}{{end}}

{{define "index"}}{{template "head" .}}
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}

<h2>Enabled repositories</h2>
{{if .Enabled}}
<table>
<thead><tr><th>Repository</th><th>Status</th><th>Trigger</th><th>Last run</th><th></th></tr></thead>
<tbody>
{{range .Enabled}}
<tr>
<td><a href="/runs?repo={{.FullName}}"><code>{{.FullName}}</code></a></td>
<td><span data-repo="{{.FullName}}" class="state-{{.Status.State}}">{{.Status.State}}</span></td>
<td>{{if .Status.Trigger}}{{.Status.Trigger}}{{else}}<span class="muted">-</span>{{end}}</td>
<td>{{if .Status.LastFinish.IsZero}}<span class="muted">-</span>{{else}}{{fmtTime .Status.LastFinish}}{{end}}</td>
<td>
<form method="post" action="/repos/run"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="repo" value="{{.FullName}}"><button class="button">Run now</button></form>
<form method="post" action="/repos/disable"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="repo" value="{{.FullName}}"><button class="button">Disable</button></form>
</td>
</tr>
{{end}}
</tbody>
</table>
{{else}}<p class="muted">No repositories enabled yet.</p>{{end}}

<p><a class="button" href="/repos">Browse repositories</a></p>

<script>
(function () {
  async function poll() {
    try {
      var response = await fetch('/api/status', { headers: { accept: 'application/json' } });
      if (!response.ok) return;
      var data = await response.json();
      var cells = document.querySelectorAll('[data-repo]');
      for (var i = 0; i < cells.length; i++) {
        var status = data[cells[i].dataset.repo];
        if (!status) continue;
        var text = status.state + (status.queued ? ' +' + status.queued : '');
        if (cells[i].textContent !== text) {
          cells[i].textContent = text;
          cells[i].className = 'state-' + status.state;
        }
      }
    } catch (e) {}
  }
  setInterval(poll, 5000);
})();
</script>
{{template "foot" .}}{{end}}

{{define "repos"}}{{template "head" .}}
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<h2>Available repositories</h2>
<p><a href="/">Back to enabled repositories</a></p>
<p><input type="search" id="filter" placeholder="Filter repositories" autocomplete="off"></p>
{{if .Other}}
<table id="other">
<thead><tr><th>Repository</th><th>Default branch</th><th></th></tr></thead>
<tbody>
{{range .Other}}
<tr>
<td><code>{{.FullName}}</code></td>
<td>{{.DefaultBranch}}</td>
<td><form method="post" action="/repos/enable"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="repo" value="{{.FullName}}"><button class="button">Enable</button></form></td>
</tr>
{{end}}
</tbody>
</table>
{{else}}<p class="muted">No other repositories you administer.</p>{{end}}

<script>
(function () {
  var filter = document.getElementById('filter');
  if (!filter) return;
  filter.addEventListener('input', function () {
    var needle = filter.value.toLowerCase();
    var rows = document.querySelectorAll('#other tbody tr');
    for (var i = 0; i < rows.length; i++) {
      rows[i].style.display = rows[i].textContent.toLowerCase().indexOf(needle) >= 0 ? '' : 'none';
    }
  });
})();
</script>
{{template "foot" .}}{{end}}

{{define "runs"}}{{template "head" .}}
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<h2>Run history{{if .RepoFilter}} for <code>{{.RepoFilter}}</code>{{end}}</h2>
{{if .RepoFilter}}<p><a href="/runs">Show all runs</a></p>{{end}}
{{if .Runs}}
<table>
<thead><tr><th>Started</th><th>Repository</th><th>Trigger</th><th>Exit</th><th>Duration</th><th>Run</th></tr></thead>
<tbody>
{{range .Runs}}
<tr>
<td>{{fmtTime .StartedAt}}</td>
<td><a href="/runs?repo={{.Repo}}"><code>{{.Repo}}</code></a></td>
<td>{{.Trigger}}{{if gt .Attempt 1}} (attempt {{.Attempt}}){{end}}</td>
<td>{{if .Running}}<span class="muted">running</span>{{else if .Success}}<span class="ok">0</span>{{else}}<span class="fail">{{.ExitCode}}</span>{{end}}</td>
<td>{{fmtDuration .Duration}}</td>
<td><a href="/runs/{{.ID}}">{{.ID}}</a></td>
</tr>
{{end}}
</tbody>
</table>
{{else}}<p class="muted">No runs yet.</p>{{end}}
{{template "foot" .}}{{end}}

{{define "run"}}{{template "head" .}}
<h2>Run {{.Run.ID}}</h2>
<p><a href="/runs?repo={{.Run.Repo}}"><code>{{.Run.Repo}}</code></a> · trigger {{.Run.Trigger}} · attempt {{.Run.Attempt}} · {{if .Run.Running}}<span class="muted">running</span>{{else}}exit {{.Run.ExitCode}}{{end}} · {{fmtDuration .Run.Duration}}</p>
{{if .Run.Error}}<p class="error">{{.Run.Error}}</p>{{end}}
{{if .Run.Running}}<p class="muted">The run is still in progress; this page refreshes every 3 seconds.</p><script>setTimeout(function(){location.reload()},3000)</script>{{end}}
<pre>{{.Log}}</pre>
{{template "foot" .}}{{end}}
`
