// spareci.dev: theme toggle, copy buttons and the savings calculator.
// No dependencies, no network requests.
(function () {
  "use strict";

  // Theme toggle. Follows the OS until the visitor picks one.
  var root = document.documentElement;
  var themeBtn = document.querySelector(".theme");
  var media = window.matchMedia("(prefers-color-scheme: dark)");
  function current() { return root.dataset.theme || (media.matches ? "dark" : "light"); }
  function paintToggle() {
    var now = current();
    root.dataset.themeNow = now;
    themeBtn.setAttribute("aria-label", now === "dark" ? "Switch to light theme" : "Switch to dark theme");
  }
  themeBtn.hidden = false;
  themeBtn.addEventListener("click", function () {
    var next = current() === "dark" ? "light" : "dark";
    root.dataset.theme = next;
    try { localStorage.setItem("theme", next); } catch (e) {}
    paintToggle();
  });
  if (media.addEventListener) media.addEventListener("change", paintToggle);
  paintToggle();

  // Copy buttons.
  if (navigator.clipboard) {
    document.querySelectorAll(".code").forEach(function (block) {
      var btn = block.querySelector(".copy");
      btn.hidden = false;
      btn.setAttribute("aria-label", "Copy command");
      btn.addEventListener("click", function () {
        var text = block.querySelector("code").textContent
          .split("\n").map(function (l) { return l.replace(/\s+#.*$/, ""); }).join("\n");
        navigator.clipboard.writeText(text).then(function () {
          btn.textContent = "Copied";
          setTimeout(function () { btn.textContent = "Copy"; }, 1500);
        });
      });
    });
  }

  // Savings calculator. Prices come from assets/prices.js, generated from
  // internal/estimate/prices.go. A runner fits a machine with the same OS and
  // CPU architecture, as in spareci estimate.
  var P = window.SPARECI_PRICES;
  var form = document.getElementById("calc");
  if (!P || !form) return;
  form.hidden = false;

  var bySku = {};
  P.runners.forEach(function (r) { bySku[r.sku] = r; });

  var checked = new Date(P.checked + "T00:00:00Z");
  document.querySelectorAll(".prices-checked").forEach(function (el) {
    el.setAttribute("datetime", P.checked);
    el.textContent = checked.toLocaleDateString("en-US", { year: "numeric", month: "long", day: "numeric", timeZone: "UTC" });
  });
  document.querySelectorAll(".prices-source").forEach(function (a) { a.href = P.source; });
  document.querySelectorAll("[data-price]").forEach(function (el) {
    var r = bySku[el.dataset.price];
    if (r) el.textContent = "$" + r.per_minute.toFixed(3);
  });

  var usd2 = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 2 });
  var usd0 = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", maximumFractionDigits: 0 });
  var num = new Intl.NumberFormat("en-US");
  function money(v) { return v >= 100 ? usd0.format(Math.round(v)) : usd2.format(v); }
  function rate(v) { return "$" + v.toFixed(3) + "/min"; }

  // Options grouped the way people think about them.
  var groups = [
    ["Standard: Linux", function (r) { return !r.larger && r.os === "Linux"; }],
    ["Standard: macOS", function (r) { return !r.larger && r.os === "macOS"; }],
    ["Standard: Windows", function (r) { return !r.larger && r.os === "Windows"; }],
    ["Larger: Linux", function (r) { return r.larger && r.os === "Linux"; }],
    ["Larger: macOS", function (r) { return r.larger && r.os === "macOS"; }],
    ["Larger: Windows", function (r) { return r.larger && r.os === "Windows"; }]
  ];
  function label(r) { return r.name + " · " + rate(r.per_minute); }
  function selectHTML(id, chosen) {
    var html = '<select id="' + id + '">';
    groups.forEach(function (g) {
      html += '<optgroup label="' + g[0] + '">';
      P.runners.filter(g[1]).forEach(function (r) {
        html += '<option value="' + r.sku + '"' + (r.sku === chosen ? " selected" : "") + ">" + label(r) + "</option>";
      });
      html += "</optgroup>";
    });
    return html + "</select>";
  }

  var rowsEl = document.getElementById("rows");
  var seq = 0;
  function addRow(sku, minutes, focus) {
    seq++;
    var li = document.createElement("li");
    li.className = "row";
    li.innerHTML =
      '<div class="c-runner"><label class="field-label" for="r' + seq + '">Runner</label>' + selectHTML("r" + seq, sku) + "</div>" +
      '<div class="c-mins"><label class="field-label" for="m' + seq + '">Minutes / month</label>' +
      '<input id="m' + seq + '" type="number" inputmode="numeric" min="0" step="1" value="' + minutes + '"></div>' +
      '<div class="c-cost"><span class="field-label">GitHub cost</span><span class="cost">$0</span></div>' +
      '<div class="c-fit"><span class="fit"></span></div>' +
      '<div class="c-rm"><button type="button" class="rm">×</button></div>';
    var sel = li.querySelector("select"), inp = li.querySelector("input");
    sel.setAttribute("aria-label", "Runner type, row " + seq);
    inp.setAttribute("aria-label", "Minutes per month, row " + seq);
    li.querySelector(".rm").setAttribute("aria-label", "Remove row " + seq);
    li.querySelector(".rm").addEventListener("click", function () {
      var next = li.nextElementSibling || li.previousElementSibling;
      li.remove();
      if (next) next.querySelector("select").focus(); else document.getElementById("add-row").focus();
      update();
    });
    rowsEl.appendChild(li);
    if (focus) sel.focus();
  }

  function hosts() {
    return Array.prototype.map.call(form.querySelectorAll('input[name="host"]:checked'), function (i) { return i.value; });
  }

  function update() {
    var hs = hosts(), total = 0, take = 0, mins = 0;
    rowsEl.querySelectorAll(".row").forEach(function (li) {
      var r = bySku[li.querySelector("select").value];
      var m = Math.max(0, Math.floor(Number(li.querySelector("input").value) || 0));
      var cost = m * r.per_minute;
      var fits = hs.indexOf(r.os + "/" + r.arch) >= 0;
      total += cost;
      if (fits) { take += cost - m * P.selfHosted; mins += m; }
      li.querySelector(".cost").textContent = money(cost);
      var f = li.querySelector(".fit");
      f.className = "fit " + (fits ? "yes" : "no");
      f.textContent = fits ? "Yes" : "No";
      f.title = fits ? "A machine you picked has this OS and CPU" : (r.os === "Windows" ? "spareci runs on macOS and Linux" : "No machine you picked has " + r.os + " " + r.arch);
    });
    var share = total > 0 ? take / total : 0;
    document.getElementById("out-cost").textContent = money(total);
    document.getElementById("out-save").textContent = money(take);
    document.getElementById("out-share").textContent = Math.round(share * 100) + "% of the bill";
    document.getElementById("out-year").textContent = money(take * 12);
    document.getElementById("out-mins").textContent = num.format(mins) + " minutes a month moved";
    document.getElementById("bar-take").style.width = (share * 100).toFixed(1) + "%";
    document.getElementById("bar").setAttribute("aria-label", "spareci could take " + Math.round(share * 100) + "% of a " + money(total) + " monthly bill");
  }

  // Defaults match the example in the README: a repo with 2,091 macOS and
  // 969 Linux minutes, about $135 a month.
  addRow("actions_macos", 2091);
  addRow("actions_linux", 969);
  addRow("actions_windows", 0);
  addRow("macos_xl", 0);

  form.addEventListener("input", update);
  form.addEventListener("change", update);
  form.addEventListener("submit", function (e) { e.preventDefault(); });
  document.getElementById("add-row").addEventListener("click", function () { addRow("linux_8_core", 0, true); update(); });
  update();
})();
