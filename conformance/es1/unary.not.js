function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(!true,false);check(!false,true);check(!0,true);check(!'',true);check(!!'x',true);console.log('PASS');
