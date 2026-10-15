// notecast: page uploads with progress, the audio player, and
// lock-screen metadata. Everything else is plain forms and htmx.
"use strict";

// --- uploads: one request per photo, in order, each with its own progress ---
document.addEventListener("change", (ev) => {
  const input = ev.target;
  if (!input.matches("input[data-upload]")) return;
  const files = Array.from(input.files || []);
  if (!files.length) return;
  const list = document.getElementById("upload-list");
  const url = input.dataset.upload;
  input.disabled = true;
  (async () => {
    let failed = 0;
    for (const f of files) {
      const li = document.createElement("li");
      li.innerHTML = '<span class="name"></span><span class="state">waiting</span>';
      li.querySelector(".name").textContent = f.name;
      const bar = document.createElement("progress");
      bar.max = 100; bar.value = 0;
      li.appendChild(bar);
      list.appendChild(li);
      const ok = await uploadOne(url, f, bar, li.querySelector(".state"));
      if (!ok) failed++;
    }
    if (failed === 0) location.reload();
    else { input.disabled = false; input.value = ""; }
  })();
});

function uploadOne(url, file, bar, state) {
  return new Promise((resolve) => {
    const xhr = new XMLHttpRequest();
    const form = new FormData();
    form.append("page", file);
    xhr.open("POST", url);
    xhr.upload.onprogress = (e) => { if (e.lengthComputable) bar.value = (e.loaded / e.total) * 100; };
    xhr.onload = () => {
      // A redirect to the note page means the page was stored.
      const ok = xhr.status >= 200 && xhr.status < 400;
      state.textContent = ok ? "done" : (xhr.responseText.trim() || "failed (" + xhr.status + ")");
      bar.value = ok ? 100 : 0;
      resolve(ok);
    };
    xhr.onerror = () => { state.textContent = "network error"; resolve(false); };
    state.textContent = "uploading";
    xhr.send(form);
  });
}

// --- player: speed buttons, chapter seeking, remembered speed ---
function setupPlayer(root) {
  const audio = root.querySelector("audio");
  if (!audio || audio.dataset.ready) return;
  audio.dataset.ready = "1";
  let saved = 1;
  try { saved = parseFloat(localStorage.getItem("notecast.speed")) || 1; } catch (_) {}
  const buttons = root.querySelectorAll(".speeds button");
  const apply = (rate) => {
    audio.playbackRate = rate;
    audio.defaultPlaybackRate = rate;
    buttons.forEach((b) => b.setAttribute("aria-pressed", String(parseFloat(b.dataset.rate) === rate)));
  };
  apply(saved);
  buttons.forEach((b) => b.addEventListener("click", () => {
    const r = parseFloat(b.dataset.rate);
    apply(r);
    try { localStorage.setItem("notecast.speed", String(r)); } catch (_) {}
  }));
  // Some browsers reset the rate when media loads.
  audio.addEventListener("loadedmetadata", () => { audio.playbackRate = audio.defaultPlaybackRate; });
  root.querySelectorAll("[data-seek]").forEach((b) => b.addEventListener("click", () => {
    audio.currentTime = parseFloat(b.dataset.seek);
    audio.play();
  }));
  audio.addEventListener("play", () => {
    if (!("mediaSession" in navigator)) return;
    navigator.mediaSession.metadata = new MediaMetadata({
      title: root.dataset.title || "notecast",
      artist: root.dataset.collection || "notecast",
      album: "notecast",
      artwork: [{ src: "/static/icon-512.png", sizes: "512x512", type: "image/png" }],
    });
    navigator.mediaSession.setActionHandler("seekbackward", (d) => { audio.currentTime -= (d.seekOffset || 15); });
    navigator.mediaSession.setActionHandler("seekforward", (d) => { audio.currentTime += (d.seekOffset || 15); });
  });
}

document.querySelectorAll(".player").forEach(setupPlayer);
document.addEventListener("htmx:load", (e) => e.detail.elt.querySelectorAll?.(".player").forEach(setupPlayer));
