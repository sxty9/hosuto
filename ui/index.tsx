import { ServerIcon, type ServicePlugin } from '@holistic/ui';
import { Dashboard } from './Dashboard';
import './i18n';

// hosuto's dashboard plugin. Linked into holistic/frontend/external/hosuto at install time and
// discovered by the host SPA's build-time registry. id MUST equal the link dir name and the
// permissions manifest's "service" field.
//
// No `visible` gate: hosuto's rights follow the hp_<id>_* convention, so the shell's default
// (show the tab to admins, and to anyone holding hp_hosuto_play|host|admin) is exactly right.
// hp_hosuto_play is default-on, so the tab is there for everyone who can actually play.
const plugin: ServicePlugin = {
  id: 'hosuto',
  displayName: 'Minecraft',
  icon: ServerIcon,
  order: 60,
  Component: Dashboard,
};

export default plugin;
