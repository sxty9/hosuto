// AskAi is hosuto's "Ask AI" tab: the same chat the aigentic service gives you, but bound to ONE
// game server and wired to hosuto's tools. It is a duplicate of aigentic's ChatView (message
// bubbles, a composer, Markdown answers, a "Working…" spinner — aigentic's chat does not stream, and
// neither does this) with two differences that make it agentic:
//
//   1. On the first send it mints a short-lived MCP token from hosuto, scoped to THIS server.
//   2. Each turn goes to aigentic's /run on the claude-cli engine with that token attached as an MCP
//      server plus a system prompt describing the server — so the model can actually operate it
//      (status, start/stop, whitelist, mods, files, logs) through hosuto's MCP tools.
//
// Everything else — the user's own Claude billing (subscription or API key), engine execution, the
// agentic tool loop — lives in aigentic. hosuto only supplies the tools and the binding. That is the
// Reuse-before-Build and Single-Source-of-Truth split the whole feature is built on.
import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import {
  Badge,
  Box,
  Button,
  EmptyState,
  Markdown,
  ScrollArea,
  Spinner,
  Stack,
  Text,
  Textarea,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import type { ServerView } from './types';

interface Msg {
  role: 'user' | 'assistant';
  content: string;
}

// The aigentic result envelope (a prizm Response whose Data is the aigentic Result). Only the
// fields this tab reads are typed.
interface RunResponse {
  data: { output: string; engine?: string; model?: string };
}

const SCROLL_ID = 'hosuto-ai-scroll';
const scrollEl = () => document.getElementById(SCROLL_ID);

// clean strips the trailing "Assistant:" the model may echo back from the transcript, plus any
// stray context tags — mirroring aigentic's own clean().
function clean(s: string): string {
  return s.replace(/^\s*Assistant:\s*/i, '').trim();
}

// systemPrompt binds the chat to this server. The tools describe themselves; this only sets the
// scene and the house rules (confirm before anything disruptive, this connection is already bound).
function systemPrompt(srv: ServerView): string {
  return [
    `You are the assistant for the Minecraft server "${srv.name}" (address ${srv.host}, Minecraft ${srv.mcVersion}, loader ${srv.loader}).`,
    `You can inspect and operate THIS server through the hosuto tools: status, players, logs, start/stop/restart, autostart, whitelist, join policy, mods and files.`,
    `This connection is already bound to this server, so you may omit the "server" argument on every tool.`,
    `Prefer calling a tool over guessing. Before anything disruptive — stopping or restarting the server, removing a mod, or removing a member — confirm with the user first.`,
    `Answer concisely, in the user's language.`,
  ].join(' ');
}

export function AskAi({ api, apiFor, ui, srv }: Pick<ServiceContextProps, 'api' | 'apiFor' | 'ui'> & { srv: ServerView }) {
  const t = useT();
  const [messages, setMessages] = useState<Msg[]>([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  // The MCP token is minted once per session, scoped to this server, and reused across turns.
  const tokenRef = useRef<string | null>(null);
  const prevLen = useRef(0);

  // A new message (sent or received) jumps to the bottom; the "Working…" row stays in view.
  useEffect(() => {
    const el = scrollEl();
    if (el && (messages.length > prevLen.current || busy)) el.scrollTop = el.scrollHeight;
    prevLen.current = messages.length;
  }, [messages, busy]);

  async function mcpToken(): Promise<string> {
    if (tokenRef.current) return tokenRef.current;
    const res = await api.post<{ token: string }>('mcp/token', { serverId: srv.id });
    tokenRef.current = res.token;
    return res.token;
  }

  async function send() {
    const text = input.trim();
    if (!text || busy) return;
    const next: Msg[] = [...messages, { role: 'user', content: text }];
    setMessages(next);
    setInput('');
    setBusy(true);
    try {
      const token = await mcpToken();
      // The whole conversation is sent as one transcript; the model continues after "Assistant:".
      const transcript = next.map((m) => `${m.role === 'user' ? 'User' : 'Assistant'}: ${m.content}`).join('\n\n') + '\n\nAssistant:';
      const res = await apiFor('aigentic').post<RunResponse>('run', {
        header: { kind: 'claude-cli' },
        data: {
          prompt: transcript,
          system: systemPrompt(srv),
          mcp: [{ name: 'hosuto', token }],
        },
      });
      setMessages([...next, { role: 'assistant', content: clean(res.data.output) }]);
    } catch (e) {
      const msg = (e as Error).message || '';
      // The claude-cli engine is unavailable when the user has connected no Claude credential.
      const description = /unavailable|credential|subscription|no Claude/i.test(msg) ? t('hosuto.ai.noEngine') : msg;
      ui.toast({ title: t('hosuto.ai.failed'), description, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  function onKey(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      void send();
    }
  }

  return (
    <Stack gap={3} className="h-full">
      <ScrollArea id={SCROLL_ID} className="grow max-h-[58vh] min-h-[30vh] pr-1">
        {messages.length === 0 ? (
          <EmptyState title={srv.name} description={t('hosuto.ai.empty')} />
        ) : (
          <Stack gap={4}>
            {messages.map((m, i) =>
              m.role === 'user' ? (
                <Box key={i} className="self-end max-w-[85%] rounded-md bg-accent/15 px-3 py-2">
                  <Text className="whitespace-pre-wrap leading-relaxed">{m.content}</Text>
                </Box>
              ) : (
                <Stack key={i} gap={1} className="max-w-full">
                  <Badge variant="accent">AI</Badge>
                  {m.content ? <Markdown text={m.content} /> : <Text color="secondary">{t('hosuto.ai.emptyReply')}</Text>}
                </Stack>
              ),
            )}
          </Stack>
        )}
        {busy && (
          <Stack direction="row" align="center" gap={2} className="mt-3">
            <Spinner className="h-4 w-4" />
            <Text variant="footnote" color="secondary">
              {t('hosuto.ai.thinking')}
            </Text>
          </Stack>
        )}
      </ScrollArea>

      <Stack direction="row" gap={2} align="end">
        <Stack grow>
          <Textarea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={onKey}
            rows={2}
            className="w-full"
            placeholder={t('hosuto.ai.placeholder')}
          />
        </Stack>
        <Button variant="primary" loading={busy} disabled={!input.trim()} onClick={send}>
          Send
        </Button>
      </Stack>
    </Stack>
  );
}
