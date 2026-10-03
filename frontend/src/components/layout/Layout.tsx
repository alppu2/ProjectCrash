import { GitHubIcon, LinkedInIcon } from '../icons';

interface LayoutProps {
  children: React.ReactNode;
}

const LINKS = [
  { label: 'GitHub', href: 'https://github.com/alppu2', Icon: GitHubIcon },
  {
    label: 'LinkedIn',
    href: 'https://www.linkedin.com/in/aleksi-valta-b365aa136',
    Icon: LinkedInIcon,
  },
];

function Layout({ children }: LayoutProps) {
  return (
    <div className="mx-auto flex h-dvh max-w-3xl flex-col px-4">
      <header className="flex items-center justify-between gap-4 border-b border-border py-4">
        <div className="min-w-0">
          <h1 className="text-base font-semibold tracking-tight">
            Aleksi Valta
          </h1>
          <p className="text-sm text-muted">
            Development Team Lead &amp; Senior Full-Stack Engineer
          </p>
        </div>
        <nav aria-label="Contact" className="flex shrink-0 gap-1">
          {LINKS.map(({ label, href, Icon }) => (
            <a
              key={label}
              href={href}
              aria-label={label}
              title={label}
              {...(href.startsWith('http') && {
                target: '_blank',
                rel: 'noreferrer',
              })}
              className="grid size-9 place-items-center rounded-lg text-muted transition-colors hover:bg-surface hover:text-fg focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
            >
              <Icon className="size-[18px]" />
            </a>
          ))}
        </nav>
      </header>
      <main className="flex min-h-0 flex-1 flex-col">{children}</main>
    </div>
  );
}

export default Layout;
