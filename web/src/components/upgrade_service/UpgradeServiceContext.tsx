import { createContext, useContext, useState } from "react";

export const UpgradeServiceContext = createContext(null);

export const UpgradeServiceProvider = ({ children }) => {
  const [config, setConfig] = useState(null);

  return (
    <UpgradeServiceContext.Provider
      // @ts-ignore
      value={{
        config,
        setConfig,
      }}
    >
      {children}
    </UpgradeServiceContext.Provider>
  );
};

export const useUpgradeServiceContext = () => {
  const context = useContext(UpgradeServiceContext);
  if (!context) {
    throw new Error(
      "useUpgradeServiceContext must be used within a UpgradeServiceProvider"
    );
  }
  return context;
};
