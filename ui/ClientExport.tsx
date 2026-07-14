// ClientExport hands the player the three shapes of the same mod set:
//
//   mods.zip     — the jars, for someone who already has a launcher and knows what to do with them.
//   ez2go.mrpack — the Modrinth pack format; Prism (and others) import it in one click.
//   prism.zip    — a ready Prism instance, server address already in it.
//
// Nothing is fetched into JS: api.url() builds the URL and the browser streams the download
// straight from the daemon, which is the only way a multi-hundred-megabyte pack is not a
// memory bomb in the tab.
import {
  Button,
  DownloadIcon,
  Panel,
  Stack,
  Text,
  useT,
  type ServiceContextProps,
} from '@holistic/ui';
import { loaderHasClientMods, type ServerView } from './types';

export function ClientExport({ api, srv }: Pick<ServiceContextProps, 'api'> & { srv: ServerView }) {
  const t = useT();
  const has = loaderHasClientMods(srv.loader);

  function download(kind: 'mods' | 'mrpack' | 'prism', filename: string) {
    const a = document.createElement('a');
    a.href = api.url(`servers/${srv.id}/export/${kind}`);
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
  }

  return (
    <Panel title={t('hosuto.tabExport')} className="p-4">
      <Stack gap={3}>
        <Stack direction="row" gap={2} wrap>
          <Button
            variant="primary"
            iconLeft={<DownloadIcon />}
            disabled={!has}
            onClick={() => download('mods', `${srv.slug}-mods.zip`)}
          >
            {t('hosuto.exportMods')}
          </Button>
          <Button
            variant="secondary"
            iconLeft={<DownloadIcon />}
            disabled={!has}
            onClick={() => download('mrpack', `${srv.slug}-ez2go.mrpack`)}
          >
            {t('hosuto.exportMrpack')}
          </Button>
          <Button
            variant="secondary"
            iconLeft={<DownloadIcon />}
            disabled={!has}
            onClick={() => download('prism', `${srv.slug}-prism.zip`)}
          >
            {t('hosuto.exportPrism')}
          </Button>
        </Stack>

        {/* A disabled button with no reason is a dead end. Paper runs plugins (server-side), and
            vanilla runs nothing — in both cases there is genuinely nothing to hand the player. */}
        {!has && (
          <Text variant="footnote" color="secondary">
            {srv.loader === 'paper' ? t('hosuto.paperPlugins') : t('hosuto.vanillaNoMods')}
          </Text>
        )}
      </Stack>
    </Panel>
  );
}
