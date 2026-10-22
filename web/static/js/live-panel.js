"use strict";
(() => {
  const intervalMs = 5000;
  let currentAudio = null;
  let currentButton = null;
  let refreshing = false;

  const setButton = (button, playing) => {
    if (!button || !button.isConnected) return;
    const icon = button.querySelector("i");
    if (icon) icon.className = playing ? "bi bi-stop-fill" : "bi bi-play-fill";
    const filename = button.getAttribute("data-wav-name") || "WAV";
    button.setAttribute("aria-label", (playing ? "Detener " : "Reproducir ") + filename);
    button.title = (playing ? "Detener " : "Reproducir ") + filename;
    button.classList.toggle("btn-primary", playing);
    button.classList.toggle("btn-outline-primary", !playing);
  };

  const stopAudio = () => {
    const audio = currentAudio;
    const button = currentButton;
    currentAudio = null;
    currentButton = null;
    if (audio) {
      audio.onended = null;
      audio.onerror = null;
      audio.pause();
      audio.removeAttribute("src");
      audio.load();
    }
    setButton(button, false);
  };

  document.addEventListener("click", (event) => {
    const button = event.target.closest("button.play-wav[data-wav-id]");
    if (!button) return;
    event.preventDefault();
    if (currentButton === button) {
      stopAudio();
      refresh();
      return;
    }
    stopAudio();
    const id = button.dataset.wavId;
    if (!/^\d+$/.test(id)) return;
    const audio = new Audio();
    // No src or <audio> is inserted until the user explicitly clicks Play.
    audio.preload = "none";
    currentAudio = audio;
    currentButton = button;
    setButton(button, true);
    audio.onended = () => {
      if (currentAudio === audio) {
        stopAudio();
        refresh();
      }
    };
    audio.onerror = () => {
      if (currentAudio === audio) {
        stopAudio();
        button.title = "No se pudo cargar el WAV; verifica la sesión";
      }
    };
    audio.src = "/jobs/" + encodeURIComponent(id) + "/audio";
    audio.play().catch(() => {
      if (currentAudio === audio) {
        stopAudio();
        button.title = "El navegador no pudo reproducir este WAV";
      }
    });
  });

  async function refresh() {
    if (refreshing || document.hidden || !document.querySelector("[data-live-region]")) return;
    refreshing = true;
    try {
      const res = await fetch(window.location.pathname + window.location.search, {
        credentials: "same-origin",
        cache: "no-store",
        headers: { "X-Requested-With": "LivePanel" }
      });
      if (res.redirected && new URL(res.url).pathname === "/login") {
        window.location.assign("/login");
        return;
      }
      if (!res.ok) return;
      const remote = new DOMParser().parseFromString(await res.text(), "text/html");
      for (const region of document.querySelectorAll("[data-live-region]")) {
        const name = region.getAttribute("data-live-region");
        const next = [...remote.querySelectorAll("[data-live-region]")]
          .find((candidate) => candidate.getAttribute("data-live-region") === name);
        if (!next || next.innerHTML === region.innerHTML) continue;
        // Keep active audio, text selection and any in-progress interaction intact.
        if (region.contains(currentButton) ||
            [...region.querySelectorAll("audio")].some((audio) => !audio.paused) ||
            region.contains(document.activeElement) &&
            document.activeElement?.matches("input,textarea,select")) continue;
        region.innerHTML = next.innerHTML;
      }
    } catch (_) {
      // Network may disconnect temporarily; the following cycle retries.
    } finally {
      refreshing = false;
    }
  }

  if (document.querySelector("[data-live-region]")) {
    window.setInterval(refresh, intervalMs);
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden) refresh();
    });
  }
})();
