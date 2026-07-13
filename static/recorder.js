/**
 * Shared audio/video recorder dialog.
 *
 * Extracted from the respond editor so any page (answers, the Ramble
 * journal) can record with the same flow: permission → start/pause/stop →
 * the take uploads for an HTTP-served review (blob: URLs frequently fail to
 * decode inline) → Save keeps it, Discard/Cancel delete it again so an
 * unkept take never lingers.
 *
 * Usage:
 *   POLRecorder.record(kind, {
 *     upload:  async (file) => ({ id, url }),  // persist the take
 *     discard: async (id) => {},               // delete an unkept take
 *     onSaved: () => {},                       // after the user keeps it
 *     toast:   (msg) => {},                    // non-blocking notice
 *   });
 */
(function () {
    function preferredRecorderType(kind) {
        const candidates = kind === 'video'
            ? ['video/webm;codecs=vp9,opus', 'video/webm;codecs=vp8,opus', 'video/webm', 'video/mp4']
            : ['audio/webm;codecs=opus', 'audio/webm', 'audio/ogg;codecs=opus', 'audio/mp4'];
        if (!window.MediaRecorder) return '';
        return candidates.find(type => MediaRecorder.isTypeSupported(type)) || '';
    }

    function recorderExtension(type, kind) {
        if (type.includes('mp4')) return kind === 'audio' ? 'm4a' : 'mp4';
        if (type.includes('ogg')) return 'ogg';
        return 'webm';
    }

    function recorderDialog() {
        let dialog = document.getElementById('record-dialog');
        if (dialog) return dialog;

        dialog = document.createElement('dialog');
        dialog.id = 'record-dialog';
        dialog.className = 'record-dialog';
        dialog.innerHTML = `
          <div class="record-dialog-body">
            <h3 data-record-title>Record</h3>
            <p class="record-status" data-record-status role="status" aria-live="polite">Waiting for permission…</p>
            <div class="record-stage">
              <video class="record-preview" data-record-preview autoplay muted playsinline hidden></video>
              <div class="record-playback" data-record-playback hidden></div>
            </div>
            <div class="record-readout">
              <span class="record-dot" data-record-dot aria-hidden="true"></span>
              <span class="record-timer" data-record-timer>0:00</span>
            </div>
            <div class="record-meter" aria-hidden="true"><span></span></div>
            <div class="record-dialog-actions">
              <button type="button" class="pl-btn pl-btn--outline" data-record-cancel>Cancel</button>
              <button type="button" class="pl-btn pl-btn--outline" data-record-discard hidden>Discard &amp; redo</button>
              <button type="button" class="pl-btn pl-btn--outline" data-record-pause hidden>Pause</button>
              <button type="button" class="pl-btn pl-btn--gold" data-record-start disabled>● Start recording</button>
              <button type="button" class="pl-btn pl-btn--gold" data-record-stop hidden>■ Stop</button>
              <button type="button" class="pl-btn pl-btn--gold" data-record-save hidden>Save recording</button>
            </div>
          </div>
        `;
        document.body.appendChild(dialog);
        return dialog;
    }

    function formatDuration(ms) {
        const total = Math.floor(ms / 1000);
        const mins = Math.floor(total / 60);
        const secs = total % 60;
        return `${mins}:${String(secs).padStart(2, '0')}`;
    }

    // One recording session. Nothing is kept until the user hits Save.
    async function record(kind, opts) {
        const toast = opts.toast || (() => {});

        if (!navigator.mediaDevices?.getUserMedia || !window.MediaRecorder) {
            toast('Recording is not supported in this browser');
            return;
        }

        const dialog = recorderDialog();
        const el = sel => dialog.querySelector(sel);
        const title = el('[data-record-title]');
        const status = el('[data-record-status]');
        const preview = el('[data-record-preview]');
        const playback = el('[data-record-playback]');
        const timer = el('[data-record-timer]');
        const startBtn = el('[data-record-start]');
        const pauseBtn = el('[data-record-pause]');
        const stopBtn = el('[data-record-stop]');
        const saveBtn = el('[data-record-save]');
        const discardBtn = el('[data-record-discard]');
        const cancelBtn = el('[data-record-cancel]');

        let stream;
        let recorder;
        let chunks = [];
        let mimeType = preferredRecorderType(kind);
        let saveType = '';
        let savedTakeId = null;   // uploaded for review but not yet kept
        let savedTakeURL = '';
        let elapsed = 0;
        let segmentStart = 0;
        let ticker = null;
        let closed = false;

        title.textContent = kind === 'video' ? 'Record video' : 'Record audio';

        function setState(state) {
            dialog.dataset.recordState = state;
            startBtn.hidden = state !== 'idle';
            pauseBtn.hidden = state !== 'recording' && state !== 'paused';
            stopBtn.hidden = state !== 'recording' && state !== 'paused';
            saveBtn.hidden = state !== 'recorded';
            discardBtn.hidden = state !== 'recorded';
        }

        function startTicker() {
            segmentStart = performance.now();
            stopTicker();
            ticker = setInterval(() => {
                timer.textContent = formatDuration(elapsed + (performance.now() - segmentStart));
            }, 200);
        }

        function stopTicker() {
            if (ticker) {
                clearInterval(ticker);
                ticker = null;
            }
        }

        function clearPlayback() {
            playback.hidden = true;
            playback.replaceChildren();
            chunks = [];
        }

        // Fire-and-forget delete of an uploaded-but-unkept take.
        function discardTake(id) {
            if (id == null) return;
            Promise.resolve(opts.discard?.(id)).catch(() => {});
        }

        async function deleteSavedTake() {
            const id = savedTakeId;
            savedTakeId = null;
            savedTakeURL = '';
            if (id == null) return;
            try { await opts.discard?.(id); } catch {}
        }

        function teardown() {
            closed = true;
            stopTicker();
            clearPlayback();
            if (preview.srcObject) preview.srcObject = null;
            stream?.getTracks().forEach(track => track.stop());
            // A take that was uploaded for review but never saved must not linger.
            discardTake(savedTakeId);
            savedTakeId = null;
        }

        function close() {
            teardown();
            if (dialog.open) dialog.close();
        }

        function resetToIdle() {
            clearPlayback();
            elapsed = 0;
            timer.textContent = '0:00';
            if (kind === 'video' && stream) {
                preview.srcObject = stream;
                preview.hidden = false;
            }
            status.textContent = kind === 'video'
                ? 'Ready — press Start when the frame looks right.'
                : 'Ready — press Start when you are.';
            setState('idle');
            if (!startBtn.disabled) startBtn.focus();
        }

        dialog.oncancel = ev => {
            ev.preventDefault();
            cancelBtn.click();
        };
        cancelBtn.onclick = () => close();

        startBtn.onclick = () => {
            chunks = [];
            recorder = new MediaRecorder(stream, mimeType ? { mimeType } : undefined);
            recorder.addEventListener('dataavailable', ev => {
                if (ev.data && ev.data.size > 0) chunks.push(ev.data);
            });
            recorder.addEventListener('stop', async () => {
                if (closed) return;
                stopTicker();
                timer.textContent = formatDuration(elapsed);

                // Drop the ";codecs=…" suffix so the stored file gets a clean
                // container MIME (matches what the server serves back).
                const rawType = recorder.mimeType || mimeType || (kind === 'video' ? 'video/webm' : 'audio/webm');
                saveType = rawType.split(';')[0].trim() || (kind === 'video' ? 'video/webm' : 'audio/webm');
                const take = new Blob(chunks, { type: saveType });

                if (preview.srcObject) preview.srcObject = null;
                preview.hidden = true;

                status.textContent = 'Processing your recording…';
                setState('processing');
                try {
                    const ext = recorderExtension(saveType, kind);
                    const file = new File([take], `${kind}-recording.${ext}`, { type: saveType });
                    const uploaded = await opts.upload(file);
                    const takeId = uploaded?.id ?? null;

                    if (closed) { discardTake(takeId); return; }

                    savedTakeId = takeId;
                    savedTakeURL = uploaded?.url || '';

                    const media = document.createElement(kind === 'video' ? 'video' : 'audio');
                    media.className = 'record-playback-media';
                    media.controls = true;
                    media.playsInline = true;
                    media.preload = 'metadata';
                    media.src = savedTakeURL;
                    playback.replaceChildren(media);
                    playback.hidden = false;
                    media.load();

                    status.textContent = 'Review your take, then Save — or Discard & redo.';
                    setState('recorded');
                } catch (err) {
                    status.textContent = err.message || 'Could not process the recording — try again.';
                    if (kind === 'video' && stream && !closed) {
                        preview.srcObject = stream;
                        preview.hidden = false;
                    }
                    setState('idle');
                    if (!startBtn.disabled) startBtn.focus();
                }
            });

            recorder.start(1000);
            elapsed = 0;
            startTicker();
            status.textContent = 'Recording…';
            setState('recording');
        };

        pauseBtn.onclick = () => {
            if (recorder?.state === 'recording') {
                recorder.pause();
                elapsed += performance.now() - segmentStart;
                stopTicker();
                timer.textContent = formatDuration(elapsed);
                pauseBtn.textContent = 'Resume';
                status.textContent = 'Paused.';
                setState('paused');
            } else if (recorder?.state === 'paused') {
                recorder.resume();
                startTicker();
                pauseBtn.textContent = 'Pause';
                status.textContent = 'Recording…';
                setState('recording');
            }
        };

        stopBtn.onclick = () => {
            if (recorder?.state === 'recording') {
                elapsed += performance.now() - segmentStart;
            }
            pauseBtn.textContent = 'Pause';
            if (recorder && recorder.state !== 'inactive') recorder.stop();
        };

        discardBtn.onclick = async () => {
            discardBtn.disabled = true;
            status.textContent = 'Discarding…';
            await deleteSavedTake();
            discardBtn.disabled = false;
            resetToIdle();
        };

        saveBtn.onclick = () => {
            if (savedTakeId == null) return;
            // The take is already uploaded; just keep it (clear the id so
            // teardown won't delete it) and hand off to the caller.
            savedTakeId = null;
            close();
            opts.onSaved?.();
        };

        setState('idle');
        status.textContent = 'Waiting for permission…';
        startBtn.disabled = true;
        preview.hidden = true;
        playback.hidden = true;
        timer.textContent = '0:00';
        dialog.showModal();

        try {
            stream = await navigator.mediaDevices.getUserMedia(
                kind === 'video' ? { audio: true, video: true } : { audio: true },
            );
            if (closed) {
                stream.getTracks().forEach(track => track.stop());
                return;
            }
            startBtn.disabled = false;
            resetToIdle();
        } catch (err) {
            close();
            toast(err.name === 'NotAllowedError'
                ? 'Microphone/camera permission was denied'
                : 'Could not start recording');
        }
    }

    window.POLRecorder = { record };
})();
