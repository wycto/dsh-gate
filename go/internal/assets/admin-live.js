/* 管理台自动刷新：轮询 /_portal/api/admin/live，只改会变的那几格。
 *
 * 为什么不是整页刷新：页面里有正在填的输入框、展开的折叠面板、选中的分页
 * （单选钮）和滚动位置，meta refresh 或 location.reload 会把它们全部丢掉。
 * 所以这里只按 data-* 钩子找到状态节点，改文本、class、hidden 三样，
 * 绝不替换表单与控件——正在填安装目录的人不会被打断。
 *
 * 安全：服务端按 sha256 放行这一个脚本（CSP script-src），数据走同源 GET
 * （connect-src 'self'），页面其余部分仍是 default-src 'none'。
 * 环境探测要真去 exec 一遍，只在「运行环境」页可见时做，且至少间隔 10 秒；
 * 标签页在后台时整个轮询暂停。
 */
(function () {
  "use strict";

  var body = document.body;
  var endpoint = body.getAttribute("data-live");
  if (!endpoint || !window.fetch || !window.JSON) {
    return;
  }

  var POLL_MS = 3000;
  var ENV_MS = 10000;
  var RETRY_MS = 15000;
  var timer = null;
  var failures = 0;
  var lastEnvAt = 0;
  var auditRev = body.getAttribute("data-audit-rev") || "0";

  function setText(node, text) {
    if (node && node.textContent !== text) {
      node.textContent = text;
    }
  }

  function show(node, visible) {
    if (node) {
      node.hidden = !visible;
    }
  }

  function tagClass(status) {
    return status ? "tag " + status : "tag";
  }

  function applyStats(stats) {
    if (!stats) {
      return;
    }
    var numbers = ["instances", "running", "users", "audit"];
    for (var i = 0; i < numbers.length; i++) {
      setText(document.querySelector('[data-stat="' + numbers[i] + '"]'), String(stats[numbers[i]]));
    }
    setText(document.querySelector('[data-stat-note="instances"]'), stats.instancesNote || "");
    setText(document.querySelector('[data-stat-note="running"]'), stats.runningNote || "");
  }

  function applyInstance(card, inst) {
    card.classList.toggle("is-ready", !!inst.ready);
    card.classList.toggle("is-starting", !inst.ready && !!inst.running);

    var node = card.querySelector('[data-badge="run"]');
    if (node) {
      node.className = tagClass(inst.ready ? "on" : inst.running ? "warn" : "off");
      setText(node, inst.runLabel || "");
    }
    node = card.querySelector('[data-badge="source"]');
    if (node) {
      setText(node, inst.runSource || "");
      show(node, !!inst.runSource);
    }
    node = card.querySelector('[data-badge="installed"]');
    if (node) {
      node.className = tagClass(inst.installed ? "on" : "off");
      setText(node, inst.installed ? inst.installedLabel : inst.notReadyLabel);
    }
    node = card.querySelector('[data-badge="phase"]');
    if (node) {
      setText(node, inst.jobPhase || "");
      show(node, !!inst.jobPhase);
    }

    var pidWrap = card.querySelector("[data-pid-wrap]");
    if (pidWrap) {
      show(pidWrap, !!inst.pid);
      setText(pidWrap.querySelector("[data-pid]"), String(inst.pid || ""));
    }
    var cmd = card.querySelector('[data-field="cmd"]');
    if (cmd && inst.command) {
      setText(cmd, inst.command);
    }

    var runNote = card.querySelector('[data-note="run"]');
    if (runNote) {
      setText(runNote.querySelector(".mono"), inst.runError || "");
      show(runNote, !!inst.runError);
    }
    var jobNote = card.querySelector('[data-note="job"]');
    if (jobNote) {
      setText(jobNote, inst.jobNote || "");
      show(jobNote, !!inst.jobNote);
    }

    var start = card.querySelector('[data-act="start"]');
    var stop = card.querySelector('[data-act="stop"]');
    var restart = card.querySelector('[data-act="restart"]');
    show(start, !inst.running);
    show(stop, !!inst.running);
    show(restart, !!inst.running);
  }

  function applyInstances(list) {
    if (!list) {
      return;
    }
    var byId = {};
    for (var i = 0; i < list.length; i++) {
      byId[list[i].id] = list[i];
    }
    var cards = document.querySelectorAll("[data-inst]");
    for (var j = 0; j < cards.length; j++) {
      var inst = byId[cards[j].getAttribute("data-inst")];
      if (inst) {
        applyInstance(cards[j], inst);
      }
    }
  }

  function applyEnv(env) {
    if (!env || !env.tools) {
      return;
    }
    var byName = {};
    for (var i = 0; i < env.tools.length; i++) {
      byName[env.tools[i].name] = env.tools[i];
    }
    var rows = document.querySelectorAll("[data-tool]");
    for (var j = 0; j < rows.length; j++) {
      var tool = byName[rows[j].getAttribute("data-tool")];
      if (!tool) {
        continue;
      }
      var node = rows[j].querySelector("[data-tool-status]");
      if (node) {
        node.className = tagClass(tool.statusClass);
        setText(node, tool.statusText || "");
      }
      node = rows[j].querySelector("[data-tool-path]");
      if (node) {
        setText(node, tool.pathText || "");
      }
      var form = rows[j].querySelector('[data-tool-action="install"]');
      if (form) {
        show(form, !!tool.showInstall);
        setText(form.querySelector("[data-tool-button]"), tool.buttonLabel || "");
      }
      show(rows[j].querySelector('[data-tool-action="during"]'), !!tool.showDuring);
    }
  }

  function addCell(row, text, cls, title, label) {
    var cell = document.createElement("td");
    if (cls) {
      cell.className = cls;
    }
    if (title) {
      cell.title = title;
    }
    if (label) {
      cell.setAttribute("data-label", label);
    }
    cell.textContent = text || "";
    row.appendChild(cell);
  }

  function applyAudit(audit) {
    if (!audit) {
      return;
    }
    var rev = String(audit.rev || 0);
    if (rev === auditRev) {
      return;
    }
    auditRev = rev;
    var table = document.querySelector("[data-audit-table]");
    var tbody = table && table.tBodies ? table.tBodies[0] : null;
    if (tbody) {
      while (tbody.firstChild) {
        tbody.removeChild(tbody.firstChild);
      }
      // 窄屏的表格会把表头折进每个单元格（td::before 读 data-label），
      // 所以重建行时把 thead 的列名抄下来，否则手机上的审计表只剩一串值。
      var heads = table.tHead && table.tHead.rows.length ? table.tHead.rows[0].cells : [];
      var rows = audit.rows || [];
      for (var i = 0; i < rows.length; i++) {
        var tr = document.createElement("tr");
        var labels = [];
        for (var c = 0; c < heads.length; c++) {
          labels.push(heads[c].textContent);
        }
        addCell(tr, rows[i].at, "mono", "", labels[0]);
        addCell(tr, rows[i].actor, "", "", labels[1]);
        addCell(tr, rows[i].actionLabel, "", rows[i].action, labels[2]);
        addCell(tr, rows[i].target, "", "", labels[3]);
        addCell(tr, rows[i].detailLabel, "path", rows[i].detail, labels[4]);
        tbody.appendChild(tr);
      }
      show(table, rows.length > 0);
      show(document.querySelector("[data-audit-empty]"), rows.length === 0);
    }
  }

  function envVisible() {
    var tab = document.getElementById("tab-env");
    return !!(tab && tab.checked);
  }

  function nextURL() {
    var query = "";
    var now = Date.now();
    if (envVisible() && now - lastEnvAt >= ENV_MS) {
      lastEnvAt = now;
      query = "?env=1";
    }
    return endpoint + query;
  }

  function schedule(ms) {
    if (timer) {
      clearTimeout(timer);
    }
    timer = setTimeout(poll, ms);
  }

  function poll() {
    timer = null;
    if (document.hidden) {
      schedule(POLL_MS);
      return;
    }
    fetch(nextURL(), {
      credentials: "same-origin",
      cache: "no-store",
      headers: { Accept: "application/json" }
    })
      .then(function (resp) {
        var type = resp.headers.get("Content-Type") || "";
        // 会话过期时网关会 303 到登录页：那是 HTML，停在这里别把登录页当 JSON。
        if (!resp.ok || resp.redirected || type.indexOf("application/json") < 0) {
          throw new Error("unexpected response");
        }
        return resp.json();
      })
      .then(function (payload) {
        failures = 0;
        applyStats(payload.stats);
        applyInstances(payload.instances);
        applyEnv(payload.env);
        applyAudit(payload.audit);
        schedule(POLL_MS);
      })
      .catch(function () {
        failures++;
        schedule(failures > 2 ? RETRY_MS : POLL_MS);
      });
  }

  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) {
      if (timer) {
        clearTimeout(timer);
        timer = null;
      }
      poll();
    }
  });

  schedule(POLL_MS);
})();
