/* ────────────────────────────────────────────
   Conversation Storage (localStorage)
   ──────────────────────────────────────────── */

class ConversationStore {
    static KEY = 'accretion-conversations';
    static ACTIVE_KEY = 'accretion-active-conversation';

    /** @returns {{ id: string, name: string, lastAccessed: string, history: object[] }[]} */
    static list() {
        try {
            const raw = localStorage.getItem(this.KEY);
            return raw ? JSON.parse(raw) : [];
        } catch {
            return [];
        }
    }

    static MAX_CONVERSATIONS = 50;

    static save(conversations) {
        // Enforce max conversation limit by removing oldest entries
        if (conversations.length > this.MAX_CONVERSATIONS) {
            conversations.sort((a, b) => new Date(b.lastAccessed) - new Date(a.lastAccessed));
            conversations.length = this.MAX_CONVERSATIONS;
        }
        try {
            localStorage.setItem(this.KEY, JSON.stringify(conversations));
        } catch (e) {
            console.warn('[ConversationStore] Failed to save — storage quota may be exceeded:', e);
            // Attempt to free space by halving stored conversations
            try {
                conversations.sort((a, b) => new Date(b.lastAccessed) - new Date(a.lastAccessed));
                conversations.length = Math.max(1, Math.floor(conversations.length / 2));
                localStorage.setItem(this.KEY, JSON.stringify(conversations));
            } catch (e2) {
                console.error('[ConversationStore] Could not save even after pruning:', e2);
            }
        }
    }

    static getActiveId() {
        return localStorage.getItem(this.ACTIVE_KEY) || null;
    }

    static setActiveId(id) {
        if (id) {
            localStorage.setItem(this.ACTIVE_KEY, id);
        } else {
            localStorage.removeItem(this.ACTIVE_KEY);
        }
    }

    static generateId() {
        return crypto.randomUUID ? crypto.randomUUID() : Date.now().toString(36) + Math.random().toString(36).slice(2);
    }

    /** Build a short summary from the first user message. */
    static summarize(history) {
        const firstUser = history.find(m => m.role === 'user');
        if (!firstUser) return 'New Conversation';
        const text = firstUser.content.trim();
        if (text.length <= 60) return text;
        return text.slice(0, 57) + '\u2026';
    }

    /** Upsert a conversation into the list. */
    static upsert(id, history) {
        const conversations = this.list();
        const idx = conversations.findIndex(c => c.id === id);
        const entry = {
            id,
            name: this.summarize(history),
            lastAccessed: new Date().toISOString(),
            history,
        };
        if (idx >= 0) {
            conversations[idx] = entry;
        } else {
            conversations.unshift(entry);
        }
        this.save(conversations);
        return entry;
    }

    static remove(id) {
        const conversations = this.list().filter(c => c.id !== id);
        this.save(conversations);
    }

    static get(id) {
        return this.list().find(c => c.id === id) || null;
    }
}

/* ────────────────────────────────────────────
   Chat Interface
   ──────────────────────────────────────────── */

class ChatInterface {
    constructor() {
        this.logLevel = window.ACCRETION_LOG_LEVEL || 'info';
        this.conversationHistory = [];
        this.activeConversationId = null;
        this.initializeElements();
        this.bindEvents();
        this.checkAuthStatus();
        this.restoreOrStartConversation();
        this.log('ChatInterface initialized');
    }

    /* ── Logging ── */

    debug(message, ...args) { if (this.shouldLog('debug')) console.log(`[DEBUG] ${message}`, ...args); }
    log(message, ...args) { if (this.shouldLog('info')) console.log(`[INFO] ${message}`, ...args); }
    warn(message, ...args) { if (this.shouldLog('warn')) console.warn(`[WARN] ${message}`, ...args); }
    error(message, ...args) { if (this.shouldLog('error')) console.error(`[ERROR] ${message}`, ...args); }

    shouldLog(level) {
        const levels = { debug: 0, info: 1, warn: 2, error: 3, none: 4 };
        return (levels[level] || 1) >= (levels[this.logLevel] || 1);
    }

    /* ── DOM Setup ── */

    initializeElements() {
        this.queryInput = document.getElementById('queryInput');
        this.sendButton = document.getElementById('sendButton');
        this.stopButton = document.getElementById('stopButton');
        this.responseArea = document.getElementById('responseArea')
            || document.querySelector('.response-area')
            || document.querySelector('[data-response-area]');
        this.loadingIndicator = document.getElementById('loadingIndicator');
        this.newConversationBtn = document.getElementById('newConversationBtn');
        this.testScrollBtn = document.getElementById('testScrollBtn');
        this.conversationListEl = document.getElementById('conversationList');
        this.abortController = null;
        this.activeTypingTimer = null;

        // Auth elements
        this.loginBtn = document.getElementById('loginBtn');
        this.logoutBtn = document.getElementById('logoutBtn');
        this.userSection = document.getElementById('userSection');
        this.authSection = document.getElementById('authSection');
        this.userName = document.getElementById('userName');
        this.userEmail = document.getElementById('userEmail');
        this.userAvatar = document.getElementById('userAvatar');
    }

    bindEvents() {
        this.sendButton?.addEventListener('click', () => this.sendMessage());

        this.newConversationBtn?.addEventListener('click', () => this.startNewConversation());

        this.testScrollBtn?.addEventListener('click', () => this.testScrollFunctionality());

        this.queryInput?.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                this.sendMessage();
            }
        });

        this.queryInput?.addEventListener('input', () => {
            this.queryInput.style.height = 'auto';
            this.queryInput.style.height = Math.min(this.queryInput.scrollHeight, 200) + 'px';
        });

        this.stopButton?.addEventListener('click', () => this.stopGenerating());

        this.loginBtn?.addEventListener('click', () => { window.location.href = '/auth/login'; });
        this.logoutBtn?.addEventListener('click', () => { window.location.href = '/auth/logout'; });

        // Suggested prompt buttons (delegated)
        this.responseArea?.addEventListener('click', (e) => {
            const btn = e.target.closest('.suggested-prompt');
            if (btn) {
                this.queryInput.value = btn.dataset.prompt;
                this.queryInput.dispatchEvent(new Event('input'));
                this.sendMessage();
            }
        });
    }

    /* ── Conversation Management ── */

    restoreOrStartConversation() {
        const activeId = ConversationStore.getActiveId();
        if (activeId) {
            const conv = ConversationStore.get(activeId);
            if (conv) {
                this.loadConversation(conv);
                this.renderConversationList();
                return;
            }
        }
        // No saved active conversation -- start fresh
        this.activeConversationId = ConversationStore.generateId();
        ConversationStore.setActiveId(this.activeConversationId);
        this.renderConversationList();
    }

    loadConversation(conv) {
        this.activeConversationId = conv.id;
        this.conversationHistory = conv.history.slice();
        ConversationStore.setActiveId(conv.id);

        // Rebuild the response area from history
        this.responseArea.innerHTML = '';
        if (this.conversationHistory.length === 0) {
            this.responseArea.innerHTML = this.welcomeHTML();
        } else {
            for (const msg of this.conversationHistory) {
                this.addMessageToDOM(msg.role === 'user' ? 'user' : 'assistant', msg.content, msg.timestamp);
            }
        }
        this.renderConversationList();
    }

    persistCurrentConversation() {
        if (!this.activeConversationId || this.conversationHistory.length === 0) return;
        ConversationStore.upsert(this.activeConversationId, this.conversationHistory);
    }

    startNewConversation() {
        // Save current conversation first
        this.persistCurrentConversation();

        // Start a blank conversation
        this.activeConversationId = ConversationStore.generateId();
        this.conversationHistory = [];
        ConversationStore.setActiveId(this.activeConversationId);

        this.responseArea.innerHTML = this.welcomeHTML();
        this.queryInput.value = '';
        this.queryInput.style.height = 'auto';
        this.queryInput.focus();
        this.renderConversationList();
        this.log('New conversation started');
    }

    renderConversationList() {
        if (!this.conversationListEl) return;
        const conversations = ConversationStore.list();

        if (conversations.length === 0) {
            this.conversationListEl.innerHTML = '<li class="conversation-list-empty">No saved conversations yet</li>';
            return;
        }

        // Sort by lastAccessed descending
        conversations.sort((a, b) => new Date(b.lastAccessed) - new Date(a.lastAccessed));

        this.conversationListEl.innerHTML = conversations.map(conv => {
            const isActive = conv.id === this.activeConversationId;
            const dateStr = this.formatLocalTimestamp(conv.lastAccessed);
            return `<li class="conversation-item${isActive ? ' active' : ''}"
                        tabindex="0"
                        role="button"
                        data-conversation-id="${conv.id}"
                        aria-current="${isActive ? 'true' : 'false'}">
                <div class="conversation-item-header">
                    <div class="conversation-item-name">${this.escapeHtml(conv.name)}</div>
                    <button class="conversation-delete-btn" data-delete-id="${conv.id}" title="Delete conversation" aria-label="Delete conversation">&times;</button>
                </div>
                <div class="conversation-item-time">${dateStr}</div>
            </li>`;
        }).join('');

        // Bind click/keyboard events
        this.conversationListEl.querySelectorAll('.conversation-item').forEach(el => {
            const handler = () => {
                const id = el.dataset.conversationId;
                if (id === this.activeConversationId) return;
                this.persistCurrentConversation();
                const conv = ConversationStore.get(id);
                if (conv) this.loadConversation(conv);
            };
            el.addEventListener('click', handler);
            el.addEventListener('keydown', (e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault();
                    handler();
                }
            });
        });

        // Bind delete buttons
        this.conversationListEl.querySelectorAll('.conversation-delete-btn').forEach(btn => {
            btn.addEventListener('click', (e) => {
                e.stopPropagation();
                const id = btn.dataset.deleteId;
                ConversationStore.remove(id);
                if (id === this.activeConversationId) {
                    this.startNewConversation();
                } else {
                    this.renderConversationList();
                }
            });
        });
    }

    formatLocalTimestamp(isoString) {
        try {
            const date = new Date(isoString);
            return new Intl.DateTimeFormat(undefined, {
                month: 'short',
                day: 'numeric',
                hour: 'numeric',
                minute: '2-digit',
            }).format(date);
        } catch {
            return '';
        }
    }

    escapeHtml(text) {
        const el = document.createElement('span');
        el.textContent = text;
        return el.innerHTML;
    }

    /* ── Helpers ── */

    welcomeHTML() {
        return `<div class="welcome-message">
            <div class="welcome-icon" aria-hidden="true">
                <svg width="40" height="40" viewBox="0 0 24 24" fill="none"><path d="M12 2L2 7l10 5 10-5-10-5zM2 17l10 5 10-5M2 12l10 5 10-5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>
            </div>
            <h2 class="welcome-title">How can I help?</h2>
            <p class="welcome-subtitle">Ask about your Kubernetes logs, metrics, and cluster health.</p>
            <div class="suggested-prompts" role="list" aria-label="Suggested prompts">
                <button class="suggested-prompt" role="listitem" data-prompt="Show me recent error logs across all namespaces">Show me recent error logs</button>
                <button class="suggested-prompt" role="listitem" data-prompt="Which pods have restarted the most in the last hour?">Which pods restarted recently?</button>
                <button class="suggested-prompt" role="listitem" data-prompt="Summarize the health of my cluster">Summarize cluster health</button>
            </div>
        </div>`;
    }

    avatarHTML(sender) {
        if (sender === 'user') {
            return `<div class="message-avatar" aria-hidden="true">
                <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor"><path d="M8 8a3 3 0 1 0 0-6 3 3 0 0 0 0 6zm5 6a5 5 0 0 0-10 0h10z"/></svg>
            </div>`;
        }
        return `<div class="message-avatar" aria-hidden="true">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none"><path d="M12 2L2 7l10 5 10-5-10-5zM2 17l10 5 10-5M2 12l10 5 10-5" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>
        </div>`;
    }

    stopGenerating() {
        if (this.abortController) {
            this.abortController.abort();
            this.abortController = null;
        }
        if (this.activeTypingTimer) {
            clearTimeout(this.activeTypingTimer);
            this.activeTypingTimer = null;
        }
        // Remove typing indicator if present
        this.responseArea.querySelector('.typing-indicator')?.remove();
        this.setLoading(false);
    }

    showTypingIndicator() {
        const existing = this.responseArea.querySelector('.typing-indicator');
        if (existing) return;
        const indicator = document.createElement('div');
        indicator.className = 'typing-indicator';
        indicator.innerHTML = `
            <div class="typing-dots" aria-hidden="true"><span></span><span></span><span></span></div>
            <span>Accretion is thinking\u2026</span>
        `;
        indicator.setAttribute('aria-live', 'polite');
        this.responseArea.appendChild(indicator);
        this.smoothScrollToBottom();
    }

    removeTypingIndicator() {
        this.responseArea.querySelector('.typing-indicator')?.remove();
    }

    /** Add copy buttons to all <pre> blocks inside a message content element. */
    attachCopyButtons(contentEl) {
        contentEl.querySelectorAll('pre').forEach(pre => {
            const btn = document.createElement('button');
            btn.className = 'copy-code-btn';
            btn.textContent = 'Copy';
            btn.setAttribute('aria-label', 'Copy code to clipboard');
            btn.addEventListener('click', () => {
                const code = pre.querySelector('code')?.textContent || pre.textContent;
                navigator.clipboard.writeText(code).then(() => {
                    btn.textContent = 'Copied!';
                    btn.classList.add('copied');
                    setTimeout(() => { btn.textContent = 'Copy'; btn.classList.remove('copied'); }, 2000);
                });
            });
            pre.style.position = 'relative';
            pre.appendChild(btn);
        });
    }

    /** Add a copy-full-message button to an assistant message element. */
    attachMessageCopyButton(messageDiv, rawContent) {
        const btn = document.createElement('button');
        btn.className = 'copy-message-btn';
        btn.setAttribute('aria-label', 'Copy message');
        btn.innerHTML = '<svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 010 1.5h-1.5a.25.25 0 00-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 00.25-.25v-1.5a.75.75 0 011.5 0v1.5A1.75 1.75 0 019.25 16h-7.5A1.75 1.75 0 010 14.25v-7.5z"/><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0114.25 11h-7.5A1.75 1.75 0 015 9.25v-7.5zm1.75-.25a.25.25 0 00-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 00.25-.25v-7.5a.25.25 0 00-.25-.25h-7.5z"/></svg>';
        btn.addEventListener('click', () => {
            navigator.clipboard.writeText(rawContent).then(() => {
                btn.classList.add('copied');
                btn.innerHTML = '<svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor"><path d="M13.78 4.22a.75.75 0 010 1.06l-7.25 7.25a.75.75 0 01-1.06 0L2.22 9.28a.75.75 0 011.06-1.06L6 10.94l6.72-6.72a.75.75 0 011.06 0z"/></svg>';
                setTimeout(() => {
                    btn.classList.remove('copied');
                    btn.innerHTML = '<svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 010 1.5h-1.5a.25.25 0 00-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 00.25-.25v-1.5a.75.75 0 011.5 0v1.5A1.75 1.75 0 019.25 16h-7.5A1.75 1.75 0 010 14.25v-7.5z"/><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0114.25 11h-7.5A1.75 1.75 0 015 9.25v-7.5zm1.75-.25a.25.25 0 00-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 00.25-.25v-7.5a.25.25 0 00-.25-.25h-7.5z"/></svg>';
                }, 2000);
            });
        });
        messageDiv.appendChild(btn);
    }

    /* ── Messaging ── */

    async sendMessage() {
        if (!this.queryInput || !this.responseArea) return;

        const query = this.queryInput.value.trim();
        if (!query) return;

        // Remove welcome message
        const welcome = this.responseArea.querySelector('.welcome-message');
        if (welcome) welcome.remove();

        this.addMessageWithTyping('user', query);
        this.conversationHistory.push({ role: 'user', content: query, timestamp: new Date().toISOString() });

        this.queryInput.value = '';
        this.queryInput.style.height = 'auto';
        this.setLoading(true);
        this.showTypingIndicator();

        // Persist after user message so the conversation appears in the list
        this.persistCurrentConversation();
        this.renderConversationList();

        this.abortController = new AbortController();

        try {
            const response = await fetch('/api/chat', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ messages: this.conversationHistory, stream: false }),
                signal: this.abortController.signal,
            });

            if (!response.ok) {
                if (response.status === 401) {
                    this.addMessageWithTyping('assistant', 'Authentication required. Please log in to continue.');
                    this.checkAuthStatus();
                    return;
                }
                throw new Error(`HTTP error! status: ${response.status}`);
            }

            const data = await response.json();
            this.removeTypingIndicator();
            if (data?.content) {
                this.addMessageWithTyping('assistant', data.content, 25);
                this.conversationHistory.push({ role: 'assistant', content: data.content, timestamp: new Date().toISOString() });
                this.persistCurrentConversation();
                this.renderConversationList();
            } else {
                this.addMessageWithTyping('assistant', 'Sorry, I received an invalid response. Please try again.');
            }
        } catch (err) {
            if (err.name === 'AbortError') {
                this.log('Request aborted by user');
            } else {
                this.error('Error sending message:', err);
                this.addMessageWithTyping('assistant', 'Sorry, there was an error processing your request. Please try again.');
            }
        } finally {
            this.abortController = null;
            this.removeTypingIndicator();
            this.setLoading(false);
        }
    }

    /** Add a message to the DOM without typing effect (used for history replay). */
    addMessageToDOM(sender, content, storedTimestamp) {
        const welcome = this.responseArea.querySelector('.welcome-message');
        if (welcome) welcome.remove();

        const messageDiv = document.createElement('div');
        messageDiv.className = `message ${sender}-message`;
        const timestamp = storedTimestamp
            ? new Date(storedTimestamp).toLocaleTimeString()
            : new Date().toLocaleTimeString();
        const rendered = sender === 'assistant' ? this.renderMarkdown(content) : this.formatMessage(content);
        const markdownClass = sender === 'assistant' ? ' markdown' : '';

        messageDiv.innerHTML = `
            <div class="message-header">
                ${this.avatarHTML(sender)}
                <div class="message-label">${sender === 'user' ? 'You' : 'Accretion'}</div>
            </div>
            <div class="message-content${markdownClass}">${rendered}</div>
            <div class="timestamp">${timestamp}</div>
        `;
        this.responseArea.appendChild(messageDiv);

        if (sender === 'assistant') {
            const contentEl = messageDiv.querySelector('.message-content');
            this.attachCopyButtons(contentEl);
            this.attachMessageCopyButton(messageDiv, content);
        }
    }

    addMessageWithTyping(sender, content, typingSpeed = 30) {
        const welcome = this.responseArea.querySelector('.welcome-message');
        if (welcome) welcome.remove();

        const messageDiv = document.createElement('div');
        messageDiv.className = `message ${sender}-message`;
        const timestamp = new Date().toLocaleTimeString();

        messageDiv.innerHTML = `
            <div class="message-header">
                ${this.avatarHTML(sender)}
                <div class="message-label">${sender === 'user' ? 'You' : 'Accretion'}</div>
            </div>
            <div class="message-content"></div>
            <div class="timestamp">${timestamp}</div>
        `;
        this.responseArea.appendChild(messageDiv);

        const contentDiv = messageDiv.querySelector('.message-content');
        this.ensureVisible(messageDiv);

        if (sender === 'user') {
            contentDiv.innerHTML = this.formatMessage(content);
            setTimeout(() => this.smoothScrollToBottom(), 50);
            return;
        }

        // Typing effect for assistant
        let i = 0;
        const typeWriter = () => {
            if (i < content.length) {
                contentDiv.innerHTML = this.formatMessage(content.substring(0, i + 1));
                this.scrollToBottom();
                i++;
                this.activeTypingTimer = setTimeout(typeWriter, typingSpeed + Math.random() * 20 - 10);
            } else {
                this.activeTypingTimer = null;
                contentDiv.classList.add('markdown');
                contentDiv.innerHTML = this.renderMarkdown(content);
                this.attachCopyButtons(contentDiv);
                this.attachMessageCopyButton(messageDiv, content);
                setTimeout(() => this.smoothScrollToBottom(), 100);
            }
        };
        typeWriter();
    }

    addErrorMessage(content) {
        const errorDiv = document.createElement('div');
        errorDiv.className = 'error-message';
        errorDiv.innerHTML = `<strong>Error:</strong> ${this.escapeHtml(content)}`;
        this.responseArea.appendChild(errorDiv);
        this.ensureVisible(errorDiv);
        setTimeout(() => this.smoothScrollToBottom(), 50);
    }

    formatMessage(content) {
        return content
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;')
            .replace(/\n/g, '<br>');
    }

    renderMarkdown(rawText) {
        try {
            if (!rawText) return '';
            const html = window.marked ? window.marked.parse(rawText) : this.formatMessage(rawText);
            return window.DOMPurify ? window.DOMPurify.sanitize(html, { USE_PROFILES: { html: true } }) : html;
        } catch (e) {
            this.error('Markdown render failed:', e);
            return this.formatMessage(rawText);
        }
    }

    /* ── Loading ── */

    setLoading(isLoading) {
        if (isLoading) {
            if (this.sendButton) this.sendButton.hidden = true;
            if (this.stopButton) this.stopButton.hidden = false;
            if (this.queryInput) this.queryInput.disabled = true;
        } else {
            if (this.sendButton) { this.sendButton.hidden = false; this.sendButton.disabled = false; }
            if (this.stopButton) this.stopButton.hidden = true;
            if (this.queryInput) this.queryInput.disabled = false;
            this.queryInput?.focus();
        }
    }

    /* ── Scrolling ── */

    scrollToBottom() { this.forceScrollToBottom(); }

    smoothScrollToBottom() {
        const start = this.responseArea.scrollTop;
        const end = this.responseArea.scrollHeight - this.responseArea.clientHeight;
        const distance = end - start;
        const duration = 300;
        let startTime = null;

        const animate = (now) => {
            if (!startTime) startTime = now;
            const progress = Math.min((now - startTime) / duration, 1);
            const ease = progress < 0.5 ? 4 * progress ** 3 : (progress - 1) * (2 * progress - 2) ** 2 + 1;
            this.responseArea.scrollTop = start + distance * ease;
            if (progress < 1) requestAnimationFrame(animate);
        };
        requestAnimationFrame(animate);
    }

    ensureVisible(element) {
        if (!element) return;
        const cRect = this.responseArea.getBoundingClientRect();
        const eRect = element.getBoundingClientRect();
        if (eRect.top < cRect.top || eRect.bottom > cRect.bottom) {
            element.scrollIntoView({ behavior: 'smooth', block: 'end', inline: 'nearest' });
        }
    }

    forceScrollToBottom() {
        if (!this.responseArea) return;
        this.responseArea.scrollTop = this.responseArea.scrollHeight;
        requestAnimationFrame(() => {
            this.responseArea.scrollTop = this.responseArea.scrollHeight;
        });
    }

    /* ── Authentication ── */

    async checkAuthStatus() {
        try {
            const response = await fetch('/auth/user');
            if (response.ok) {
                this.currentUser = await response.json();
                this.updateAuthUI(true);
            } else {
                this.currentUser = null;
                this.updateAuthUI(false);
            }
        } catch {
            this.currentUser = null;
            this.updateAuthUI(false);
        }
    }

    updateAuthUI(isAuthenticated) {
        if (isAuthenticated && this.currentUser) {
            if (this.userSection) this.userSection.style.display = 'flex';
            if (this.authSection) this.authSection.style.display = 'none';
            if (this.userName) this.userName.textContent = this.currentUser.name || 'Unknown User';
            if (this.userEmail) this.userEmail.textContent = this.currentUser.email || '';
            if (this.userAvatar && this.currentUser.picture) {
                this.userAvatar.src = this.currentUser.picture;
                this.userAvatar.style.display = 'block';
            } else if (this.userAvatar) {
                this.userAvatar.style.display = 'none';
            }
        } else {
            if (this.userSection) this.userSection.style.display = 'none';
            if (this.authSection) this.authSection.style.display = 'flex';
        }
    }

    /* ── Developer Debug (hidden in production) ── */

    testScrollFunctionality() {
        for (let i = 1; i <= 10; i++) {
            setTimeout(() => {
                this.addMessageWithTyping('assistant', `Test message ${i} - Lorem ipsum dolor sit amet, consectetur adipiscing elit.`, 5);
            }, i * 500);
        }
    }
}

// Initialize
document.addEventListener('DOMContentLoaded', () => {
    try {
        window.chat = new ChatInterface();
    } catch (error) {
        console.error('ChatInterface initialization failed:', error);
    }
});
