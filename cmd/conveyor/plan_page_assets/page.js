// Shared page script: tab switching and "Approve all unchanged". Kept tiny and
// dependency-free; every asset is embedded in the binary, so no CDN is used.
(function () {
  "use strict";

  function showTab(name) {
    var panels = document.querySelectorAll(".tab-panel");
    for (var i = 0; i < panels.length; i++) {
      panels[i].hidden = panels[i].getAttribute("data-panel") !== name;
    }
    var tabs = document.querySelectorAll(".tab");
    for (var j = 0; j < tabs.length; j++) {
      tabs[j].classList.toggle("active", tabs[j].getAttribute("data-tab") === name);
    }
  }

  function firstTab() {
    var tab = document.querySelector(".tab");
    if (tab) {
      showTab(tab.getAttribute("data-tab"));
    }
  }

  document.addEventListener("click", function (event) {
    var target = event.target;
    if (!target || !target.closest) {
      return;
    }
    var tab = target.closest(".tab");
    if (tab) {
      showTab(tab.getAttribute("data-tab"));
      return;
    }
    var approve = target.closest(".approve-all");
    if (approve) {
      var panel = document.querySelector('.tab-panel[data-panel="' + approve.getAttribute("data-tab") + '"]');
      if (!panel) {
        return;
      }
      var cards = panel.querySelectorAll(".card");
      for (var i = 0; i < cards.length; i++) {
        if (cards[i].getAttribute("data-changed") === "true") {
          continue;
        }
        var radio = cards[i].querySelector('input[type="radio"][value="approve"]');
        if (radio) {
          radio.checked = true;
        }
      }
    }
  });

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", firstTab);
  } else {
    firstTab();
  }
})();
