// AskAiThread is one conversation of the "Ask AI" tab: the message list and composer for a single
// shared, persistent thread bound to one server. It is the aigentic ChatView, adapted so the thread
// lives in hosuto (shared across operators) and the run is agentic against hosuto's MCP tools.
//
// The model call runs in the browser (billed to the sending operator's own aigentic account); on
// success the exchange — with which engine/model answered — is appended to the shared conversation
// and the sidebar is nudged to re-sort. The conversation is polled so operators see each other's
// turns land.
import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import {
  Avatar,
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
  useLiveQuery,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import { faceUrl } from './face';
import type { ChatMsg, Conversation, ServerView } from './types';

interface RunResponse {
  data: { output: string; engine?: string; model?: string };
}

const ENGINE_LABEL: Record<string, string> = {
  'claude-cli': 'Claude CLI',
  'claude-api': 'Claude API',
  ollama: 'Local',
  choose: 'Auto',
};
const engineLabel = (e?: string) => (e ? (ENGINE_LABEL[e] ?? e) : 'AI');

const scrollId = (convId: string) => `hosuto-ai-scroll-${convId}`;

function clean(s: string): string {
  return s.replace(/^\s*Assistant:\s*/i, '').trim();
}

function systemPrompt(srv: ServerView): string {
  return [
    `You are the assistant for the Minecraft server "${srv.name}" (address ${srv.host}, Minecraft ${srv.mcVersion}, loader ${srv.loader}).`,
    `You can inspect and operate THIS server through the hosuto tools: status, players, logs, start/stop/restart, autostart, whitelist, join policy, mods and files.`,
    `This connection is already bound to this server, so you may omit the "server" argument on every tool.`,
    `This is a shared operator chat — more than one person may be talking to you. Prefer calling a tool over guessing. Before anything disruptive — stopping or restarting the server, removing a mod, or removing a member — confirm with the user first.`,
    `Answer concisely, in the user's language.`,
  ].join(' ');
}

export function AskAiThread({
  api,
  apiFor,
  ui,
  user,
  srv,
  convId,
  getToken,
  onPosted,
}: Pick<ServiceContextProps, 'api' | 'apiFor' | 'ui' | 'user'> & {
  srv: ServerView;
  convId: string;
  getToken: () => Promise<string>;
  onPosted: () => void;
}) {
  const t = useT();
  const q = useLiveQuery<Conversation>(() => api.get<Conversation>(`servers/${srv.id}/chats/${convId}`), 5000, [srv.id, convId]);
  const history: ChatMsg[] = q.data?.messages ?? [];

  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState<string | null>(null);
  const prevLen = useRef(0);

  useEffect(() => {
    const el = document.getElementById(scrollId(convId));
    const total = history.length + (pending ? 1 : 0);
    if (el && (total > prevLen.current || busy)) el.scrollTop = el.scrollHeight;
    prevLen.current = total;
  }, [history.length, pending, busy, convId]);

  async function send() {
    const text = input.trim();
    if (!text || busy) return;
    setPending(text);
    setInput('');
    setBusy(true);
    try {
      const token = await getToken();
      const transcript =
        [...history, { role: 'user', content: text }].map((m) => `${m.role === 'user' ? 'User' : 'Assistant'}: ${m.content}`).join('\n\n') + '\n\nAssistant:';
      const res = await apiFor('aigentic').post<RunResponse>('run', {
        header: { kind: 'claude-cli' },
        data: { prompt: transcript, system: systemPrompt(srv), mcp: [{ name: 'hosuto', token }] },
      });
      const answer = clean(res.data.output);
      await api.post(`servers/${srv.id}/chats/${convId}`, { user: text, assistant: answer, engine: res.data.engine, model: res.data.model });
      setPending(null);
      q.refresh();
      onPosted();
    } catch (e) {
      setPending(null);
      const msg = (e as Error).message || '';
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

  const empty = history.length === 0 && !pending;

  return (
    <Stack gap={3} className="h-full">
      <ScrollArea id={scrollId(convId)} className="grow max-h-[54vh] min-h-[30vh] pr-1">
        {empty ? (
          <EmptyState title={srv.name} description={t('hosuto.ai.empty')} />
        ) : (
          <Stack gap={4}>
            {history.map((m, i) =>
              m.role === 'user' ? (
                <Stack key={i} gap={1} align="end">
                  {m.author && (
                    <Stack direction="row" align="center" gap={2}>
                      <Text variant="caption" color="tertiary">
                        {m.name || m.author}
                      </Text>
                      <Avatar name={m.name || m.author} src={faceUrl(api, m.author, 40)} size={20} />
                    </Stack>
                  )}
                  <Box className="self-end max-w-[85%] rounded-md bg-accent/15 px-3 py-2">
                    <Text className="whitespace-pre-wrap leading-relaxed">{m.content}</Text>
                  </Box>
                </Stack>
              ) : (
                <Stack key={i} gap={1} className="max-w-full">
                  <Stack direction="row" align="center" gap={2}>
                    <Badge variant="accent">{engineLabel(m.engine)}</Badge>
                    {m.model && (
                      <Text variant="caption" color="tertiary">
                        {m.model}
                      </Text>
                    )}
                  </Stack>
                  {m.content ? <Markdown text={m.content} /> : <Text color="secondary">{t('hosuto.ai.emptyReply')}</Text>}
                </Stack>
              ),
            )}
            {pending && (
              <Stack gap={1} align="end">
                <Stack direction="row" align="center" gap={2}>
                  <Text variant="caption" color="tertiary">
                    {user.username}
                  </Text>
                  <Avatar name={user.username} src={faceUrl(api, user.username, 40)} size={20} />
                </Stack>
                <Box className="self-end max-w-[85%] rounded-md bg-accent/15 px-3 py-2 opacity-70">
                  <Text className="whitespace-pre-wrap leading-relaxed">{pending}</Text>
                </Box>
              </Stack>
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
